package logbuf

import (
	"fmt"
	"testing"
	"time"
)

func TestKeepsTheNewestLines(t *testing.T) {
	b := New(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(b, "line %d\n", i)
	}
	got := b.Since(0, 10)
	if len(got) != 3 || got[0].Text != "line 3" || got[2].Text != "line 5" || got[2].ID != 5 {
		t.Fatalf("lines %+v", got)
	}
	if got := b.Since(4, 10); len(got) != 1 || got[0].Text != "line 5" {
		t.Errorf("since 4: %+v", got)
	}
	if got := b.Since(1, 10); len(got) != 3 || got[0].ID != 3 {
		t.Errorf("a caller that is behind gets what is left: %+v", got)
	}
	if got := b.Since(0, 2); len(got) != 2 || got[0].Text != "line 4" {
		t.Errorf("the newest two: %+v", got)
	}
}

func TestLinesSplitAcrossWrites(t *testing.T) {
	b := New(10)
	b.Write([]byte("one\ntw"))
	b.Write([]byte("o\r\nthree"))
	got := b.Since(0, 10)
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("lines %+v", got)
	}
	b.Write([]byte("\n"))
	if got := b.Since(2, 10); len(got) != 1 || got[0].Text != "three" {
		t.Errorf("the rest: %+v", got)
	}
}

func TestSubscribersAreTold(t *testing.T) {
	b := New(10)
	c, cancel := b.Subscribe()
	defer cancel()
	b.Write([]byte("hello\n"))
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("not told")
	}
	cancel()
	b.Write([]byte("again\n")) // no subscriber: nothing blocks
}
