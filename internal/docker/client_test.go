package docker

import (
	"bytes"
	"testing"
)

func frame(stream byte, s string) []byte {
	n := len(s)
	return append([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, s...)
}

func TestDemux(t *testing.T) {
	in := bytes.NewReader(append(append(frame(1, "out1 "), frame(2, "err")...), frame(1, "out2")...))
	var out, errOut bytes.Buffer
	if err := Demux(in, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out1 out2" || errOut.String() != "err" {
		t.Errorf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func TestCapped(t *testing.T) {
	b := &capped{max: 4}
	n, _ := b.Write([]byte("abcdef"))
	if n != 6 || b.String() != "abcd" {
		t.Errorf("got %d %q", n, b.String())
	}
}
