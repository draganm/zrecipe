package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLevelOrder(t *testing.T) {
	cases := map[byte][]int{
		2: {9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		4: {1, 2, 3, 4, 5, 6, 7, 8, 9, 0},
		0: {6, 5, 7, 4, 8, 3, 9, 2, 1, 0},
		7: {6, 5, 7, 4, 8, 3, 9, 2, 1, 0},
	}
	for xfl, want := range cases {
		if got := LevelOrder(xfl); !reflect.DeepEqual(got, want) {
			t.Errorf("xfl %d: got %v want %v", xfl, got, want)
		}
	}
}

func TestGzipParamsJSONIsFlat(t *testing.T) {
	p := GzipParams{HeaderB64: "H4sI", DeflateParams: DeflateParams{Level: 6, Strategy: StrategyDefault, WindowBits: 15, MemLevel: 8}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"header_b64":"H4sI","level":6,"strategy":"default","window_bits":15,"mem_level":8}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	var back GzipParams
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestZstdParamsJSONOmitsOptional(t *testing.T) {
	b, err := json.Marshal(ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"level":3,"checksum":true,"content_size":true,"pledged_size":true,"single_segment":false,"workers":0}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}

func TestModuleVersionUnknown(t *testing.T) {
	if v := ModuleVersion("example.com/does/not/exist"); v != "(devel)" {
		t.Fatalf("got %q", v)
	}
}

type fakeEngine struct{ name string }

func (f fakeEngine) Name() string    { return f.name }
func (f fakeEngine) Version() string { return "v0" }
func (f fakeEngine) Format() Format  { return FormatGzip }

func TestByName(t *testing.T) {
	engines := []Engine{fakeEngine{"a"}, fakeEngine{"b"}}
	if e, ok := ByName(engines, "b"); !ok || e.Name() != "b" {
		t.Fatalf("got %v %v", e, ok)
	}
	if _, ok := ByName(engines, "c"); ok {
		t.Fatal("found c")
	}
}
