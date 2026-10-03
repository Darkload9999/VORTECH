package career

import "testing"

func ladder() Levels {
	return NewLevels([]Level{
		{Rank: 2, Code: "ANALYST_II", MinXP: 300},
		{Rank: 0, Code: "INTERN", MinXP: 0},
		{Rank: 1, Code: "ANALYST_I", MinXP: 100},
	})
}

func TestForXP(t *testing.T) {
	tests := []struct {
		xp       int32
		wantCode string
		wantNext string
	}{
		{0, "INTERN", "ANALYST_I"},
		{99, "INTERN", "ANALYST_I"},
		{100, "ANALYST_I", "ANALYST_II"},
		{299, "ANALYST_I", "ANALYST_II"},
		{300, "ANALYST_II", ""},
		{100000, "ANALYST_II", ""},
	}
	for _, tt := range tests {
		cur, next := ladder().ForXP(tt.xp)
		if cur.Code != tt.wantCode {
			t.Errorf("ForXP(%d) = %s, want %s", tt.xp, cur.Code, tt.wantCode)
		}
		gotNext := ""
		if next != nil {
			gotNext = next.Code
		}
		if gotNext != tt.wantNext {
			t.Errorf("ForXP(%d) next = %q, want %q", tt.xp, gotNext, tt.wantNext)
		}
	}
	if cur, next := (Levels{}).ForXP(10); cur.Code != "" || next != nil {
		t.Fatal("empty ladder must yield the zero level")
	}
}
