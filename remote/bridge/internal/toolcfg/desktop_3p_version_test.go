package toolcfg

import "testing"

func TestClaude3PVersionSupport(t *testing.T) {
	for _, c := range []struct {
		v                    string
		compatible, verified bool
	}{
		{"2.16120.0", true, true},
		{"2.16120.0.0", true, true},
		{"2.16120.1", true, false},
		{"2.16200.0", true, false},
		{"2.17000.3.0", true, false},
		{"2.16119.9", false, false}, // older than the contract
		{"1.99999.0", false, false},
		{"3.0.0", false, false}, // another major line is never guessed at
		{"99.0.0", false, false},
		{"2.16120", false, false},
		{"2.16120.x", false, false},
		{"", false, false},
	} {
		compatible, verified := Claude3PVersionSupport(c.v)
		if compatible != c.compatible || verified != c.verified {
			t.Errorf("%q: got %v/%v want %v/%v", c.v, compatible, verified, c.compatible, c.verified)
		}
	}
}
