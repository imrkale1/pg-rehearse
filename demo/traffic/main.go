// Command traffic simulates the demo shop's application hitting the "prod"
// database, so pg_stat_statements and pg_stat_activity have realistic content
// for `pg-rehearse profile` to capture.
//
// Mix (by request share):
//
//	checkout       10%  insert order + 1-4 items + payment, in one transaction
//	view order     30%  order by id with its items
//	order history  25%  a user's latest orders
//	product page   35%  product by id + how many times it sold
//	admin report   every -report-every: a multi-second aggregate in a REPEATABLE READ txn
//
// Usage:
//
//	go run ./demo/traffic -dsn postgres://shop:shop@localhost:55440/shop -rate 200 -duration 2m
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queries carry a leading /* feature */ comment. pg_stat_statements keeps it in
// the stored text, which makes `--feature-map 'feature:checkout=checkout'`-style
// labelling trivial in the profile step.
const (
	qInsertOrder   = `/* checkout */ INSERT INTO orders (user_id, status, total) VALUES ($1, 'pending', 0) RETURNING id`
	qProductPrice  = `/* checkout */ SELECT price FROM products WHERE id = $1`
	qInsertItem    = `/* checkout */ INSERT INTO order_items (order_id, product_id, quantity, unit_price) VALUES ($1, $2, $3, $4)`
	qInsertPayment = `/* checkout */ INSERT INTO payments (order_id, provider, provider_ref, amount, status) VALUES ($1, $2, $3, $4, 'authorized')`
	qFinishOrder   = `/* checkout */ UPDATE orders SET total = $2, status = 'paid', updated_at = now() WHERE id = $1`

	qViewOrder = `/* view_order */ SELECT o.id, o.status, o.total, o.created_at, oi.product_id, oi.quantity, oi.unit_price
FROM orders o JOIN order_items oi ON oi.order_id = o.id WHERE o.id = $1`

	qOrderHistory = `/* order_history */ SELECT id, status, total, created_at FROM orders
WHERE user_id = $1 ORDER BY created_at DESC LIMIT 20`

	qProductPage  = `/* product_page */ SELECT id, sku, name, category, price, stock, description FROM products WHERE id = $1`
	qProductSales = `/* product_page */ SELECT count(*) FROM order_items WHERE product_id = $1`

	qReportRevenue = `/* admin_report */ SELECT category, sum(revenue) AS revenue, sum(orders) AS orders
FROM daily_revenue WHERE day >= (now() - interval '365 days')::date GROUP BY category ORDER BY revenue DESC`
	qReportTopCustomers = `/* admin_report */ SELECT u.id, u.email, count(*) AS orders, sum(o.total) AS spent
FROM orders o JOIN users u ON u.id = o.user_id
GROUP BY u.id, u.email ORDER BY spent DESC LIMIT 50`
)

type kind int

const (
	kCheckout kind = iota
	kViewOrder
	kOrderHistory
	kProductPage
	kAdminReport
	numKinds
)

var kindNames = [numKinds]string{"checkout", "view_order", "order_history", "product_page", "admin_report"}

// cumulative weights for the interactive mix (admin report is scheduled separately).
var mix = []struct {
	k   kind
	cum float64
}{{kCheckout, 0.10}, {kViewOrder, 0.40}, {kOrderHistory, 0.65}, {kProductPage, 1.0}}

type stats struct {
	mu    sync.Mutex
	lat   [numKinds][]time.Duration
	errs  [numKinds]int
	last  [numKinds]string
	since time.Time // start of the current window
}

func (s *stats) record(k kind, d time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errs[k]++
		s.last[k] = err.Error()
		return
	}
	s.lat[k] = append(s.lat[k], d)
}

// snapshot returns a printable summary and resets the window.
func (s *stats) snapshot() string {
	s.mu.Lock()
	lat, errs, last := s.lat, s.errs, s.last
	window := time.Since(s.since)
	s.lat, s.errs, s.last = [numKinds][]time.Duration{}, [numKinds]int{}, [numKinds]string{}
	s.since = time.Now()
	s.mu.Unlock()

	var b strings.Builder
	total := 0
	for k := range numKinds {
		total += len(lat[k]) + errs[k]
	}
	fmt.Fprintf(&b, "%.0f req/s\n", float64(total)/window.Seconds())
	for k := range numKinds {
		l := lat[k]
		if len(l) == 0 && errs[k] == 0 {
			continue
		}
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		fmt.Fprintf(&b, "  %-14s n=%-6d err=%-3d p50=%-9s p99=%-9s max=%s\n", kindNames[k], len(l), errs[k],
			pct(l, 0.50), pct(l, 0.99), pct(l, 1))
		if last[k] != "" {
			fmt.Fprintf(&b, "  %-14s last error: %s\n", "", last[k])
		}
	}
	return b.String()
}

func pct(l []time.Duration, p float64) string {
	if len(l) == 0 {
		return "-"
	}
	i := int(p * float64(len(l)-1))
	return l[i].Round(100 * time.Microsecond).String()
}

type app struct {
	maxUser, maxOrder, maxProduct int64
}

func (a *app) do(ctx context.Context, c *pgx.Conn, k kind, rng *rand.Rand) error {
	switch k {
	case kCheckout:
		return a.checkout(ctx, c, rng)
	case kViewOrder:
		rows, err := c.Query(ctx, qViewOrder, a.hotOrder(rng))
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	case kOrderHistory:
		rows, err := c.Query(ctx, qOrderHistory, a.hotUser(rng))
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	case kProductPage:
		id := a.hotProduct(rng)
		rows, err := c.Query(ctx, qProductPage, id)
		if err != nil {
			return err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var n int64
		return c.QueryRow(ctx, qProductSales, id).Scan(&n)
	case kAdminReport:
		return adminReport(ctx, c)
	}
	return nil
}

func (a *app) checkout(ctx context.Context, c *pgx.Conn, rng *rand.Rand) error {
	return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		var orderID int64
		if err := tx.QueryRow(ctx, qInsertOrder, a.hotUser(rng)).Scan(&orderID); err != nil {
			return err
		}
		var total float64
		for range 1 + rng.IntN(4) {
			pid := a.hotProduct(rng)
			var price float64
			if err := tx.QueryRow(ctx, qProductPrice, pid).Scan(&price); err != nil {
				return err
			}
			qty := 1 + rng.IntN(3)
			if _, err := tx.Exec(ctx, qInsertItem, orderID, pid, qty, price); err != nil {
				return err
			}
			total += price * float64(qty)
		}
		provider := []string{"stripe", "stripe", "adyen", "paypal"}[rng.IntN(4)]
		ref := fmt.Sprintf("pay_%016x", rng.Uint64())
		if _, err := tx.Exec(ctx, qInsertPayment, orderID, provider, ref, total); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, qFinishOrder, orderID, total)
		return err
	})
}

// adminReport is the classic outage ingredient: a long read transaction that
// holds AccessShareLock on orders/order_items/products for several seconds.
func adminReport(ctx context.Context, c *pgx.Conn) error {
	return pgx.BeginTxFunc(ctx, c, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// Reporting replicas are rarely tuned; keep it single-threaded so it takes a while.
		if _, err := tx.Exec(ctx, "SET LOCAL max_parallel_workers_per_gather = 0"); err != nil {
			return err
		}
		for _, q := range []string{qReportRevenue, qReportTopCustomers} {
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Skewed id pickers: recent orders and active users are hotter.
func (a *app) hotOrder(rng *rand.Rand) int64 {
	return a.maxOrder - int64(float64(a.maxOrder-1)*pow3(rng.Float64()))
}
func (a *app) hotUser(rng *rand.Rand) int64 {
	return 1 + int64(float64(a.maxUser-1)*rng.Float64()*rng.Float64())
}
func (a *app) hotProduct(rng *rand.Rand) int64 {
	return 1 + int64(float64(a.maxProduct-1)*pow3(rng.Float64()))
}
func pow3(x float64) float64 { return x * x * x }

func main() {
	dsn := flag.String("dsn", envOr("PROD_DSN", "postgres://shop:shop@localhost:55440/shop?sslmode=disable"), "prod database DSN")
	rate := flag.Float64("rate", 200, "interactive requests per second (all features combined)")
	duration := flag.Duration("duration", 2*time.Minute, "how long to run (0 = until Ctrl-C)")
	workers := flag.Int("workers", 16, "concurrent connections for interactive traffic")
	reportEvery := flag.Duration("report-every", 30*time.Second, "interval between admin report runs (0 = disabled)")
	statsEvery := flag.Duration("stats-every", 10*time.Second, "how often to print latency stats")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	a, err := discover(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	log.Printf("shop has %d users, %d orders, %d products; generating %.0f req/s with %d workers",
		a.maxUser, a.maxOrder, a.maxProduct, *rate, *workers)

	st := &stats{since: time.Now()}
	jobs := make(chan kind, *workers*4)
	var dropped atomic.Int64
	var wg sync.WaitGroup

	for i := range *workers {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, 7))
			c, err := pgx.Connect(ctx, *dsn)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("worker connect: %v", err)
				}
				return
			}
			defer c.Close(context.Background())
			for k := range jobs {
				start := time.Now()
				err := a.do(ctx, c, k, rng)
				if ctx.Err() != nil {
					return
				}
				st.record(k, time.Since(start), err)
				if c.IsClosed() {
					if c, err = pgx.Connect(ctx, *dsn); err != nil {
						return
					}
				}
			}
		}(uint64(i + 1))
	}

	if *reportEvery > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := pgx.Connect(ctx, *dsn)
			if err != nil {
				return
			}
			defer c.Close(context.Background())
			t := time.NewTicker(*reportEvery)
			defer t.Stop()
			for {
				start := time.Now()
				err := adminReport(ctx, c)
				if ctx.Err() != nil {
					return
				}
				st.record(kAdminReport, time.Since(start), err)
				log.Printf("admin report took %s (err=%v)", time.Since(start).Round(time.Millisecond), err)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	// Stats printer.
	go func() {
		t := time.NewTicker(*statsEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("%s", st.snapshot())
				if d := dropped.Swap(0); d > 0 {
					log.Printf("  (%d requests dropped: workers saturated; raise -workers)", d)
				}
			}
		}
	}()

	// Open-loop generator: exponential inter-arrival times around the target rate.
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	start := time.Now()
	next := start
gen:
	for {
		next = next.Add(time.Duration(rng.ExpFloat64() / *rate * float64(time.Second)))
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
				break gen
			case <-time.After(d):
			}
		} else if ctx.Err() != nil {
			break gen
		}
		r := rng.Float64()
		k := mix[len(mix)-1].k
		for _, m := range mix {
			if r < m.cum {
				k = m.k
				break
			}
		}
		select {
		case jobs <- k:
		default:
			dropped.Add(1)
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("done after %s; final window:\n%s", time.Since(start).Round(time.Second), st.snapshot())
	if errors.Is(ctx.Err(), context.Canceled) {
		os.Exit(0)
	}
}

func discover(ctx context.Context, dsn string) (*app, error) {
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	a := &app{}
	err = c.QueryRow(ctx, `SELECT (SELECT max(id) FROM users), (SELECT max(id) FROM orders), (SELECT max(id) FROM products)`).
		Scan(&a.maxUser, &a.maxOrder, &a.maxProduct)
	if err != nil {
		return nil, fmt.Errorf("reading shop size (is the demo seeded?): %w", err)
	}
	return a, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
