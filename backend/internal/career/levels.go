package career

import "sort"

// Level is one career level. Rank is derived from XP, never stored.
type Level struct {
	Rank        int32  `json:"rank"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MinXP       int32  `json:"min_xp"`
}

// Levels is the ordered career ladder (rank 0 has min_xp 0, enforced by
// the database).
type Levels []Level

// NewLevels sorts levels by rank.
func NewLevels(ls []Level) Levels {
	out := append(Levels(nil), ls...)
	sort.Slice(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

// ForXP returns the highest level whose min_xp <= xp and the next level,
// or nil at the top of the ladder.
func (ls Levels) ForXP(xp int32) (Level, *Level) {
	if len(ls) == 0 {
		return Level{}, nil
	}
	cur := 0
	for i, l := range ls {
		if l.MinXP <= xp {
			cur = i
		}
	}
	if cur+1 < len(ls) {
		next := ls[cur+1]
		return ls[cur], &next
	}
	return ls[cur], nil
}
