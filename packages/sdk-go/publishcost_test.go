package sentinel

import (
	"strings"
	"testing"
)

func TestVarintLength(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{0, 1}, {127, 1}, {128, 2}, {16383, 2}, {16384, 3},
		{2097151, 3}, {2097152, 4}, {268435455, 4}, {268435456, 5},
	}
	for _, c := range cases {
		if got := vl(c.n); got != c.want {
			t.Errorf("vl(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestLD(t *testing.T) {
	if got := ld(0); got != 1+1+0 {
		t.Errorf("ld(0) = %d, want 2", got)
	}
	if got := ld(200); got != 1+2+200 {
		t.Errorf("ld(200) = %d, want %d", got, 1+2+200)
	}
}

func TestVValue(t *testing.T) {
	if V(nil) != 2 {
		t.Errorf("V(nil) = %d, want 2", V(nil))
	}
	if V(true) != 2 {
		t.Errorf("V(true) = %d, want 2", V(true))
	}
	if V(float64(0)) != 9 {
		t.Errorf("V(number) = %d, want 9 (an 8-byte double plus its 1-byte tag)", V(float64(0)))
	}
	if got, want := V("ab"), ld(2); got != want {
		t.Errorf("V(string) = %d, want %d", got, want)
	}
	// A number inside a list costs ld(V(number)) = ld(9) = 11 bytes, against
	// JSON's 2 ("0,") - spec T40.
	if got, want := ld(V(float64(0))), 11; got != want {
		t.Errorf("ld(V(number)) = %d, want %d (T40: a number is 11 bytes in a list)", got, want)
	}
}

func TestJSONStringExcessFastPath(t *testing.T) {
	if got := jsonStringExcess("plain ascii, no escapes"); got != 0 {
		t.Errorf("jsonStringExcess of a plain string = %d, want 0 (fast path)", got)
	}
}

func TestJSONStringExcessExactForEscapes(t *testing.T) {
	// A backslash costs 2 bytes escaped ("\\") vs 1 raw: excess 1 per backslash.
	s := strings.Repeat(`\`, 100)
	got := jsonStringExcess(s)
	if got != 100 {
		t.Errorf("jsonStringExcess(100 backslashes) = %d, want 100", got)
	}
}

func TestPublishCostNeverUnderProtobufOnBackslashHeaders(t *testing.T) {
	// The rc14 regression case: 64 headers of 4,096 backslashes must NOT be
	// counted as escaped (6x overcount) - the fix subtracts jsonStringExcess.
	headers := make(map[string]string, 64)
	for i := 0; i < 64; i++ {
		headers[strings.Repeat("h", 1)+string(rune('a'+i%26))+string(rune('0'+i/26))] = strings.Repeat(`\`, 4096)
	}
	ev := &Event{
		EventID:    "00000000-0000-4000-8000-000000000001",
		Platform:   "go",
		ErrorClass: "Error",
		Request:    &RequestInfo{Method: "GET", Headers: headers},
	}
	b := boundEvent(ev, 50)
	m := &decodeMirror{}
	cost := publishCost(b, m)
	if cost > maxPublishBytes {
		t.Fatalf("publishCost of the backslash-headers case = %d, want <= %d (the escape-overcount fix, rc14)", cost, maxPublishBytes)
	}
}
