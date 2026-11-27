package strictjson

import (
	"encoding/json"
	"errors"
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

func TestUnmarshalDecodesWhatEncodingJSONDecodes(t *testing.T) {
	for _, tc := range []struct {
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
	} {
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
