package locks

// Table-level lock modes in increasing strength, as reported by pg_locks.mode.
const (
	AccessShare          = "AccessShareLock"
	RowShare             = "RowShareLock"
	RowExclusive         = "RowExclusiveLock"
	ShareUpdateExclusive = "ShareUpdateExclusiveLock"
	Share                = "ShareLock"
	ShareRowExclusive    = "ShareRowExclusiveLock"
	Exclusive            = "ExclusiveLock"
	AccessExclusive      = "AccessExclusiveLock"
)

var modeIndex = map[string]int{
	AccessShare: 0, RowShare: 1, RowExclusive: 2, ShareUpdateExclusive: 3,
	Share: 4, ShareRowExclusive: 5, Exclusive: 6, AccessExclusive: 7,
}

// conflictMatrix[a][b] is true when lock mode a conflicts with mode b.
// Straight from the "Conflicting Lock Modes" table in the Postgres docs.
var conflictMatrix = [8][8]bool{
	//            AS     RS     RE     SUE    S      SRE    E      AE
	/* AS  */ {false, false, false, false, false, false, false, true},
	/* RS  */ {false, false, false, false, false, false, true, true},
	/* RE  */ {false, false, false, false, true, true, true, true},
	/* SUE */ {false, false, false, true, true, true, true, true},
	/* S   */ {false, false, true, true, false, true, true, true},
	/* SRE */ {false, false, true, true, true, true, true, true},
	/* E   */ {false, true, true, true, true, true, true, true},
	/* AE  */ {true, true, true, true, true, true, true, true},
}

// Conflicts reports whether two table-level lock modes conflict. Unknown
// modes are treated as conflicting with everything (fail safe).
func Conflicts(a, b string) bool {
	ia, oka := modeIndex[a]
	ib, okb := modeIndex[b]
	if !oka || !okb {
		return true
	}
	return conflictMatrix[ia][ib]
}

// BlocksReads: would a plain SELECT (AccessShareLock) wait behind this mode?
func BlocksReads(mode string) bool { return Conflicts(mode, AccessShare) }

// BlocksWrites: would INSERT/UPDATE/DELETE (RowExclusiveLock) wait behind this mode?
func BlocksWrites(mode string) bool { return Conflicts(mode, RowExclusive) }

// Stronger reports whether mode a is strictly stronger than mode b.
func Stronger(a, b string) bool { return modeIndex[a] > modeIndex[b] }
