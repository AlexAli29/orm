package orm_test

import (
	"encoding/json"
	"testing"

	"github.com/AlexAli29/orm"
)

type post struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type user struct {
	ID      int64            `json:"id"`
	Posts   orm.Many[post]   `json:"posts,omitzero"`
	Profile orm.One[profile] `json:"profile,omitzero"`
}

type profile struct {
	Bio string `json:"bio"`
}

func TestMany_states(t *testing.T) {
	var unloaded orm.Many[post]
	if _, ok := unloaded.Get(); ok {
		t.Error("zero Many reported loaded")
	}
	if !unloaded.IsZero() {
		t.Error("zero Many: IsZero = false, want true")
	}
	if got := unloaded.Len(); got != 0 {
		t.Errorf("zero Many: Len = %d, want 0", got)
	}

	empty := orm.NewMany[post]()
	vs, ok := empty.Get()
	if !ok {
		t.Fatal("NewMany() reported unloaded")
	}
	if vs == nil {
		t.Error("NewMany(): Get returned a nil slice, want non-nil empty")
	}
	if empty.IsZero() {
		t.Error("NewMany(): IsZero = true, want false")
	}

	full := orm.NewMany(post{ID: 1}, post{ID: 2})
	if got := full.Len(); got != 2 {
		t.Errorf("NewMany(2 items): Len = %d, want 2", got)
	}
	if full.MustGet()[1].ID != 2 {
		t.Errorf("NewMany: MustGet()[1].ID = %d, want 2", full.MustGet()[1].ID)
	}
}

func TestNewManyFrom_nilSliceIsLoaded(t *testing.T) {
	m := orm.NewManyFrom[post](nil)
	if m.IsZero() {
		t.Fatal("NewManyFrom(nil): IsZero = true, want false (loaded, empty)")
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Errorf("NewManyFrom(nil) encoded as %s, want []", b)
	}
}

func TestMany_mustGetPanicsWhenUnloaded(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustGet on an unloaded Many did not panic")
		}
	}()
	var m orm.Many[post]
	_ = m.MustGet()
}

func TestOne_states(t *testing.T) {
	var unloaded orm.One[profile]
	if _, ok := unloaded.Get(); ok {
		t.Error("zero One reported loaded")
	}
	if !unloaded.IsZero() {
		t.Error("zero One: IsZero = false, want true")
	}

	absent := orm.Absent[profile]()
	v, ok := absent.Get()
	if !ok {
		t.Fatal("Absent() reported unloaded")
	}
	if v != nil {
		t.Errorf("Absent(): Get returned %v, want nil", v)
	}
	if absent.IsZero() {
		t.Error("Absent(): IsZero = true, want false")
	}
	if absent.MustGet() != nil {
		t.Error("Absent(): MustGet returned non-nil")
	}

	present := orm.NewOne(&profile{Bio: "hi"})
	if got := present.MustGet().Bio; got != "hi" {
		t.Errorf("NewOne: Bio = %q, want %q", got, "hi")
	}
}

func TestOne_mustGetPanicsWhenUnloaded(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustGet on an unloaded One did not panic")
		}
	}()
	var o orm.One[profile]
	_ = o.MustGet()
}

func TestMarshalJSON_omitzeroDropsUnloaded(t *testing.T) {
	b, err := json.Marshal(user{ID: 7})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"id":7}`
	if string(b) != want {
		t.Errorf("unloaded relations encoded as %s, want %s", b, want)
	}
}

func TestMarshalJSON_loadedEmptyAndAbsent(t *testing.T) {
	u := user{
		ID:      7,
		Posts:   orm.NewMany[post](),
		Profile: orm.Absent[profile](),
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"id":7,"posts":[],"profile":null}`
	if string(b) != want {
		t.Errorf("encoded as %s, want %s", b, want)
	}
}

func TestMarshalJSON_loadedValues(t *testing.T) {
	u := user{
		ID:      7,
		Posts:   orm.NewMany(post{ID: 1, Title: "a"}),
		Profile: orm.NewOne(&profile{Bio: "b"}),
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"id":7,"posts":[{"id":1,"title":"a"}],"profile":{"bio":"b"}}`
	if string(b) != want {
		t.Errorf("encoded as %s, want %s", b, want)
	}
}

func TestUnmarshalJSON_roundTrip(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantPosts  func(orm.Many[post]) error
		wantAbsent bool
	}{
		{
			name: "absent fields stay unloaded",
			in:   `{"id":1}`,
		},
		{
			name: "empty array is loaded and empty",
			in:   `{"id":1,"posts":[],"profile":null}`,
		},
		{
			name: "values are loaded",
			in:   `{"id":1,"posts":[{"id":2,"title":"t"}],"profile":{"bio":"b"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var u user
			if err := json.Unmarshal([]byte(tt.in), &u); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			out, err := json.Marshal(u)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(out) != tt.in {
				t.Errorf("round trip produced %s, want %s", out, tt.in)
			}
		})
	}
}

func TestUnmarshalJSON_nullMany(t *testing.T) {
	var m orm.Many[post]
	if err := json.Unmarshal([]byte("null"), &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !m.IsZero() {
		t.Error("Many decoded from null is loaded, want unloaded")
	}
}

func TestUnmarshalJSON_nullOneIsAbsent(t *testing.T) {
	var o orm.One[profile]
	if err := json.Unmarshal([]byte("null"), &o); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	v, ok := o.Get()
	if !ok {
		t.Fatal("One decoded from null is unloaded, want loaded")
	}
	if v != nil {
		t.Errorf("One decoded from null holds %v, want nil", v)
	}
}

func TestUnmarshalJSON_typeErrorIsWrapped(t *testing.T) {
	var m orm.Many[post]
	if err := json.Unmarshal([]byte(`{"not":"an array"}`), &m); err == nil {
		t.Error("decoding an object into Many succeeded, want error")
	}
}
