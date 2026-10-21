package merges

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestGroupsAreKeptAndNormalised(t *testing.T) {
	p := filepath.Join(t.TempDir(), "merges.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set([][]string{{"11.1", " 11.3", "11.1"}, {"9.1"}, {"14.1", "66.2"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Groups(); !reflect.DeepEqual(got, [][]string{{"11.1", "11.3"}, {"14.1", "66.2"}}) {
		t.Fatalf("groups %v", got)
	}
	if got := s.GroupOf("11.3"); !reflect.DeepEqual(got, []string{"11.3", "11.1"}) {
		t.Errorf("the channel itself comes first: %v", got)
	}
	if s.GroupOf("9.1") != nil {
		t.Error("a channel alone is in no group")
	}
	rev := s.Rev()
	if err := s.Set([][]string{{"11.1", "11.3"}, {"11.3", "5.1"}}); err == nil {
		t.Error("a channel in two groups was accepted")
	}
	if s.Rev() != rev || len(s.Groups()) != 2 {
		t.Error("a refused change altered the groups")
	}
	again, err := Open(p)
	if err != nil || !reflect.DeepEqual(again.Groups(), s.Groups()) {
		t.Fatalf("not kept: %v %v", again.Groups(), err)
	}
}
