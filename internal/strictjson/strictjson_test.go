package strictjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/session"
)

func TestDeepBodiesUseLinearMemory(t *testing.T) {
	const depth = 3000
	key := strings.Repeat("k", 90)
	nested := strings.Repeat(`{"`+key+`":`, depth) + `0` + strings.Repeat("}", depth)
	body := []byte(`{"value":` + nested + `}`)
	for _, tc := range []struct {
		name  string
		value any
		want  error
	}{
		{"string", &struct {
			Value string `json:"value"`
		}{}, ErrInvalid},
		{"any", &struct {
			Value any `json:"value"`
		}{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := Unmarshal(body, tc.value)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error: %v, want %v", err, tc.want)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			limit := uint64(len(body)) * 64
			t.Logf("body=%d allocated=%d limit=%d", len(body), allocated, limit)
			if allocated > limit {
				t.Errorf("allocated %d bytes for %d-byte body; limit %d", allocated, len(body), limit)
			}
		})
	}
}

func TestDeepSiblingObjectsUseLinearMemory(t *testing.T) {
	var wide strings.Builder
	wide.WriteByte('{')
	for i := 0; i < 2000; i++ {
		if i > 0 {
			wide.WriteByte(',')
		}
		wide.WriteString(strconv.Quote("key" + strconv.Itoa(i)))
		wide.WriteString(":0")
	}
	wide.WriteByte('}')
	small := "[" + strings.Repeat(`{"a":0},`, 1999) + `{"a":0}]`
	for _, tc := range []struct {
		name, value string
		start, end  int
	}{
		{"wide_500_530", wide.String(), 500, 530},
		{"small_500_530", small, 500, 530},
		{"wide_8690_8710", wide.String(), 8690, 8710},
		{"small_8690_8710", small, 8690, 8710},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var maxAlloc uint64
			var maxDepth, maxSize int
			for depth := tc.start; depth <= tc.end; depth++ {
				body := []byte(`{"value":` + strings.Repeat(`{"d":`, depth) + tc.value + strings.Repeat("}", depth) + `}`)
				var got struct {
					Value string `json:"value"`
				}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				err := Unmarshal(body, &got)
				runtime.ReadMemStats(&after)
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("depth %d: got %v", depth, err)
				}
				allocated := after.TotalAlloc - before.TotalAlloc
				if depth == 8703 {
					t.Logf("depth=%d body=%d allocated=%d", depth, len(body), allocated)
				}
				if allocated > maxAlloc {
					maxAlloc, maxDepth, maxSize = allocated, depth, len(body)
				}
				if limit := uint64(len(body)) * 96; allocated > limit {
					t.Errorf("depth=%d body=%d allocated=%d limit=%d", depth, len(body), allocated, limit)
				}
			}
			t.Logf("maximum depth=%d body=%d allocated=%d", maxDepth, maxSize, maxAlloc)
		})
	}
}

func TestDeepRepeatedKeyNamesTheFullPath(t *testing.T) {
	const depth = 256
	key := `a"b\c`
	quoted := strconv.Quote(key)
	body := `{"value":` + strings.Repeat(`{`+quoted+`:`, depth) + `{"duplicate":"secret-value","duplicate":"other"}` + strings.Repeat("}", depth) + `}`
	var got struct {
		Value any `json:"value"`
	}
	err := Unmarshal([]byte(body), &got)
	want := `$["value"]` + strings.Repeat("["+quoted+"]", depth) + `["duplicate"]: ` + ErrRepeatedKey.Error()
	if !errors.Is(err, ErrRepeatedKey) || err.Error() != want {
		t.Fatalf("wrong error path: %v", err)
	}
	body = `{"value":{"first":{"inside":1},"second":{"nested":{"x":2}},"target":{"before":{"leaf":3},"dup":1,"dup":2}}}`
	err = Unmarshal([]byte(body), &got)
	want = `$["value"]["target"]["dup"]: ` + ErrRepeatedKey.Error()
	if !errors.Is(err, ErrRepeatedKey) || err.Error() != want {
		t.Fatalf("wrong sibling path: %v", err)
	}
}

type customJSON struct{ Value string }

type failingJSON struct{}

func (*failingJSON) UnmarshalJSON([]byte) error { return errors.New("private-value") }

type customText string

func (c *customText) UnmarshalText(b []byte) error {
	*c = customText(strings.ToUpper(string(b)))
	return nil
}

func (c *customJSON) UnmarshalJSON(b []byte) error {
	var v map[string]string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	c.Value = v["value"]
	return nil
}

func TestUnmarshalRefusesARepeatedKey(t *testing.T) {
	for _, body := range []string{`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`} {
		var got map[string]int
		if err := Unmarshal([]byte(body), &got); !errors.Is(err, ErrRepeatedKey) {
			t.Errorf("%s: got %v, want repeated key", body, err)
		}
	}
}

func TestUnmarshalMatchesFieldsOnlyByTheirExactName(t *testing.T) {
	type target struct {
		DryRun     bool `json:"dry_run,omitempty"`
		Rows, Cols int
		Ignored    string `json:"-"`
		Dash       string `json:"-,"`
		hidden     string
	}
	var good target
	if err := Unmarshal([]byte(`{"dry_run":true,"Rows":2,"Cols":3,"-":"yes"}`), &good); err != nil || !good.DryRun || good.Rows != 2 || good.Cols != 3 || good.Dash != "yes" {
		t.Fatalf("exact fields: %+v %v", good, err)
	}
	for _, key := range []string{"DRY_RUN", "Dry_Run", "rows", "cols", "hidden", "Ignored", "extra"} {
		var got target
		err := Unmarshal([]byte(`{"`+key+`":1}`), &got)
		if !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%s: got %v, want unknown key", key, err)
		}
	}
}

func TestUnmarshalRefusesRepeatedKeysAtEveryDepth(t *testing.T) {
	type child struct {
		A int `json:"a"`
	}
	type target struct {
		Struct child            `json:"struct"`
		Map    map[string]child `json:"map"`
		Array  []child          `json:"array"`
		Any    any              `json:"any"`
		Raw    json.RawMessage  `json:"raw"`
		Custom customJSON       `json:"custom"`
		Text   customText       `json:"text"`
	}
	for name, body := range map[string]string{
		"struct": `{"struct":{"a":1,"a":2}}`,
		"map":    `{"map":{"x":{"a":1,"a":2}}}`,
		"array":  `{"array":[{"a":1,"a":2}]}`,
		"any":    `{"any":{"z":{"a":1,"a":2}}}`,
		"raw":    `{"raw":[{"a":1,"a":2}]}`,
		"custom": `{"custom":{"value":"a","value":"b"}}`,
		"text":   `{"text":{"a":1,"a":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var v target
			if err := Unmarshal([]byte(body), &v); !errors.Is(err, ErrRepeatedKey) {
				t.Fatalf("got %v, want repeated key", err)
			}
		})
	}
}

func TestUnmarshalKeepsMapKeysAsWritten(t *testing.T) {
	var got map[string]struct {
		Value int `json:"value"`
	}
	err := Unmarshal([]byte(`{"a":{"value":1},"A":{"value":2}}`), &got)
	if err != nil || len(got) != 2 || got["a"].Value != 1 || got["A"].Value != 2 {
		t.Fatalf("map: %+v %v", got, err)
	}
	if err := Unmarshal([]byte(`{"a":{"Value":1}}`), &got); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("map value: %v", err)
	}
}

func TestUnmarshalNeedsAnObjectForAStruct(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `"x"`, `1`, ``} {
		var got struct {
			Child *struct{} `json:"child"`
		}
		want := ErrNotObject
		if body == "" {
			want = ErrInvalid
		}
		if err := Unmarshal([]byte(body), &got); !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", body, err, want)
		}
	}
	var got struct {
		Child *struct{} `json:"child"`
	}
	if err := Unmarshal([]byte(`{"child":null}`), &got); err != nil || got.Child != nil {
		t.Fatalf("nested null: %+v %v", got, err)
	}
}

func TestUnmarshalRefusesTrailingData(t *testing.T) {
	var v struct{}
	if err := Unmarshal([]byte(`{} {}`), &v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want invalid", err)
	}
}

type embedded struct{ A int }
type Exported struct{ A int }

func TestUnmarshalRefusesAnEmbeddedStruct(t *testing.T) {
	var v struct{ embedded }
	if err := Unmarshal([]byte(`{"A":1}`), &v); !errors.Is(err, ErrEmbedded) {
		t.Fatalf("got %v, want embedded", err)
	}
	var named struct {
		Exported `json:"named"`
	}
	if err := Unmarshal([]byte(`{"named":{"A":1}}`), &named); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("tagged anonymous field: %v", err)
	}
}

// validBodies are the destinations the agent decodes bodies into, with a body each accepts. The deploy and session
// requests are the real ones; the others stand for opaque and custom values.
var validBodies = []struct {
	name, body string
	newValue   func() any
}{
	{"deploy", `{"app":"api","version":"v1","commit":"c","request_id":"r","manifest":{"project":"demo","network":"overlay","protected":true,"apps":{"api":{"kind":"image","env":{"secret":["PASSWORD",{"TOKEN":"API_TOKEN"}]}}},"backups":{"db":{"retention":{"keep":7}}}}}`, func() any { return &deploy.Request{} }},
	{"session", `{"kind":"exec","app":"web","command":["sh"],"tty":true,"size":{"Rows":24,"Cols":80}}`, func() any { return &session.Request{} }},
	{"opaque", `{"raw":{"Arbitrary":true},"custom":{"value":"x"},"any":{"Case":2}}`, func() any {
		return &struct {
			Raw    json.RawMessage `json:"raw"`
			Custom customJSON      `json:"custom"`
			Any    any             `json:"any"`
		}{}
	}},
	{"text", `{"value":"hello"}`, func() any {
		return &struct {
			Value customText `json:"value"`
		}{}
	}},
}

func TestUnmarshalDecodesWhatEncodingJSONDecodes(t *testing.T) {
	for _, tc := range validBodies {
		t.Run(tc.name, func(t *testing.T) {
			want, got := tc.newValue(), tc.newValue()
			if err := json.Unmarshal([]byte(tc.body), want); err != nil {
				t.Fatal(err)
			}
			if err := Unmarshal([]byte(tc.body), got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v: %v", got, want, err)
			}
		})
	}
}

func TestUnmarshalErrorsNameKeysNotValues(t *testing.T) {
	var v struct {
		Outer struct {
			Name string `json:"name"`
		} `json:"outer"`
	}
	err := Unmarshal([]byte(`{"outer":{"name":"private-value","name":"other-secret"}}`), &v)
	if !errors.Is(err, ErrRepeatedKey) || !strings.Contains(err.Error(), "outer") || !strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "other-secret") {
		t.Errorf("unsafe or missing path: %v", err)
	}
	var bad struct {
		Custom failingJSON `json:"custom"`
	}
	err = Unmarshal([]byte(`{"custom":{"value":"private-value"}}`), &bad)
	if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("custom error disclosed a value: %v", err)
	}
	var mismatch struct {
		Outer struct {
			Number int `json:"number"`
		} `json:"outer"`
	}
	err = Unmarshal([]byte(`{"outer":{"number":"private-value"}}`), &mismatch)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "outer") || !strings.Contains(err.Error(), "number") || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("type error lacks a safe key path: %v", err)
	}
}

// bodyShapes are the destinations FuzzStrictRequestBody selects from: those of validBodies, then a map, a list and any.
func bodyShapes() []func() any {
	var shapes []func() any
	for _, c := range validBodies {
		shapes = append(shapes, c.newValue)
	}
	return append(shapes,
		func() any {
			return &map[string]struct {
				Value int `json:"value"`
			}{}
		},
		func() any {
			return &[]struct {
				A int `json:"a"`
			}{}
		},
		func() any { return new(any) },
	)
}

// isStrictError reports whether err wraps one of the exported errors, as every refusal does.
func isStrictError(err error) bool {
	for _, e := range []error{ErrInvalid, ErrNotObject, ErrRepeatedKey, ErrUnknownKey, ErrEmbedded} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// checkStrictControls runs the fixed cases of a request body: what is accepted, and what must be refused, for the two
// request types the agent decodes. It fails a decoder that accepts everything or refuses everything.
func checkStrictControls(t *testing.T) {
	t.Helper()
	for _, c := range validBodies {
		got, want := c.newValue(), c.newValue()
		if err := json.Unmarshal([]byte(c.body), want); err != nil {
			t.Fatal(err)
		}
		if err := Unmarshal([]byte(c.body), got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("valid %s body: %v", c.name, err)
		}
	}
	for _, r := range []struct {
		newValue func() any
		key      string
	}{
		{func() any { return &deploy.Request{} }, "app"},
		{func() any { return &session.Request{} }, "kind"},
	} {
		wrongCase := strings.ToUpper(r.key[:1]) + r.key[1:]
		escaped := fmt.Sprintf(`\u%04x`, r.key[0]) + r.key[1:]
		for _, c := range []struct {
			body string
			want error
		}{
			{`{"` + r.key + `":"x","` + r.key + `":"y"}`, ErrRepeatedKey},
			{`{"` + r.key + `":"x","` + escaped + `":"y"}`, ErrRepeatedKey},
			{`{"` + wrongCase + `":"x"}`, ErrUnknownKey},
			{`[]`, ErrNotObject}, {`"x"`, ErrNotObject}, {`null`, ErrNotObject}, {`1`, ErrNotObject},
			{`{} {}`, ErrInvalid}, {`{"` + r.key + `":"x"} 1`, ErrInvalid}, {`{"` + r.key + `":`, ErrInvalid},
		} {
			if err := Unmarshal([]byte(c.body), r.newValue()); !errors.Is(err, c.want) {
				t.Fatalf("%T %s: got %v, want %v", r.newValue(), c.body, err, c.want)
			}
		}
	}
	// The wire accepts every well-formed session request, and Check decides separately whether it is a good one.
	for _, c := range []struct {
		body        string
		wire, check bool
	}{
		{`{"kind":"exec","app":"web","command":["sh"]}`, true, true},
		{`{"kind":"run","app":"web","role":"w","command":["sh"],"timeout":"5m"}`, true, true},
		{`{"kind":"exec","app":"web","command":["sh"],"tty":true,"size":{"Rows":24,"Cols":80}}`, true, true},
		{`{"kind":"attach","app":"web","command":["sh"]}`, true, false},
		{`{"kind":"exec","app":"web","command":["sh"],"size":{"Rows":24,"Cols":80}}`, true, false},
		{`{"kind":"exec","app":"web","command":[""]}`, true, false},
		{`{"kind":"exec","app":"web","command":["s\u0000h"]}`, true, false},
		{`{"kind":"exec","app":"web","command":["sh"],"tty":true,"size":{"Rows":1001,"Cols":80}}`, true, false},
		{`{"kind":"exec","app":"web","command":["sh"],"tty":true,"size":{"Rows":70000,"Cols":80}}`, false, false},
		{`{"kind":"exec","app":"web","command":["sh"],"tty":true,"size":{"rows":24,"Cols":80}}`, false, false},
	} {
		var r session.Request
		err := Unmarshal([]byte(c.body), &r)
		if (err == nil) != c.wire {
			t.Fatalf("%s: wire error %v, want accepted %v", c.body, err, c.wire)
		}
		if err == nil && (r.Check() == nil) != c.check {
			t.Fatalf("%s: Check %v, want ok %v", c.body, r.Check(), c.check)
		}
	}
}

func FuzzStrictRequestBody(f *testing.F) {
	for i, c := range validBodies {
		f.Add(uint8(i), []byte(c.body))
	}
	deep := strings.Repeat(`{"d":`, 100) + "0" + strings.Repeat("}", 100)
	var wide strings.Builder
	wide.WriteByte('{')
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&wide, `"key%d":0,`, i)
	}
	wide.WriteString(`"last":0}`)
	anyShape := uint8(len(validBodies) + 2)
	f.Add(anyShape, []byte(deep))
	f.Add(anyShape, []byte(wide.String()))
	f.Add(uint8(len(validBodies)), []byte(`{"a":{"value":1},"A":{"value":2}}`))
	f.Add(uint8(len(validBodies)+1), []byte(`[{"a":1},{"a":2}]`))
	for shape := range bodyShapes() {
		for _, body := range []string{
			`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `{"A":1}`, `{"DRY_RUN":1}`, `{"app":"x","app":"y"}`, `{"kind":"exec","Kind":"run"}`,
			`null`, `[]`, `"x"`, `1`, ``, `{} {}`, `{"a":`, `{"raw":[{"a":1,"a":2}]}`, `{"custom":{"value":"a","value":"b"}}`,
			`{"manifest":{"apps":{"api":{"kind":"image","kind":"image"}}}}`,
		} {
			f.Add(uint8(shape), []byte(body))
		}
	}
	shapes := bodyShapes()
	f.Fuzz(func(t *testing.T, shape uint8, body []byte) {
		checkStrictControls(t)
		got, want := shapes[int(shape)%len(shapes)](), shapes[int(shape)%len(shapes)]()
		err := Unmarshal(body, got)
		if err != nil {
			if !isStrictError(err) {
				t.Fatalf("refusal wraps no exported error: %v", err)
			}
			return
		}
		if err := json.Unmarshal(body, want); err != nil {
			t.Fatalf("accepted a body encoding/json refuses: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded %#v, encoding/json decoded %#v", got, want)
		}
		if r, ok := got.(*session.Request); ok {
			_ = r.Check() // a decoded request is checked where it is used; the check must not panic
		}
	})
}
