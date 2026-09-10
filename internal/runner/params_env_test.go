package runner

import (
	"reflect"
	"strings"
	"testing"
)

func TestParamsEnvironment(t *testing.T) {
	params := map[string]any{
		"cluster_uuid": "中文 = 'quotes' $(echo no)\n", "dry_run": false,
		"count": 3, "ratio": 1.25, "empty": "", "optional": nil,
		"items": []any{1, true}, "options": map[string]any{"invalid-key allowed here": "\x00"},
		"PATH": "custom", "HOME": "custom", "SCRIPT_PARAMS_FILE": "custom", "_id2": "ok",
	}
	got, err := paramsEnvironment(params)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PARAM_HOME=custom", "PARAM_PATH=custom", "PARAM_SCRIPT_PARAMS_FILE=custom", "PARAM__ID2=ok",
		"PARAM_CLUSTER_UUID=中文 = 'quotes' $(echo no)\n", "PARAM_COUNT=3", "PARAM_DRY_RUN=false",
		"PARAM_EMPTY=", "PARAM_ITEMS=[1,true]", "PARAM_OPTIONAL=null",
		`PARAM_OPTIONS={"invalid-key allowed here":"\u0000"}`, "PARAM_RATIO=1.25",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParamsEnvironmentValidation(t *testing.T) {
	for _, key := range []string{"", "a-b", "a.b", "a b", "1a", "a=b", "中文", "a\n", "a\x00"} {
		if _, err := (Request{Language: "shell", Source: "true", Params: map[string]any{key: "value"}}).Normalize(); err == nil {
			t.Errorf("accepted key %q", key)
		}
	}
	for _, params := range []map[string]any{
		{"name": "one", "NAME": "two"},
		{"value": "secret\x00value"},
	} {
		if _, err := (Request{Language: "shell", Source: "true", Params: params}).Normalize(); err == nil {
			t.Errorf("accepted invalid environment params")
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("parameter value leaked in validation error")
		}
	}
}

func TestParamsEnvironmentSizeLimit(t *testing.T) {
	value := strings.Repeat("x", MaxParamsEnvBytes-len("PARAM_X=")-1)
	req := Request{Language: "shell", Source: "true", Params: map[string]any{"x": value}}
	if _, err := req.Normalize(); err != nil {
		t.Fatal("exact limit rejected:", err)
	}
	req.Params["x"] = value + "x"
	if _, err := req.Normalize(); err == nil || !strings.Contains(err.Error(), "params environment exceeds") {
		t.Fatalf("expected environment size error, got %v", err)
	}
}

func TestGoPythonParamsDoNotUseEnvironmentValidation(t *testing.T) {
	for _, language := range []string{"go", "python"} {
		for _, params := range []map[string]any{
			{"bad-key": "value", "": "empty key", "中文": "value", "name": "one", "NAME": "two", "nul": "\x00"},
			// JSON fits the request limit but the equivalent environment would not.
			{"x": strings.Repeat("x", MaxParamsEnvBytes-len("PARAM_X="))},
		} {
			req := Request{Language: language, Source: "source", Params: params}
			if _, err := req.Normalize(); err != nil {
				t.Fatalf("%s wrongly applied environment rules: %v", language, err)
			}
		}
		// The ordinary JSON size limit still applies to both languages.
		if _, err := (Request{Language: language, Source: "source", Params: map[string]any{"x": strings.Repeat("x", MaxParamsBytes)}}).Normalize(); err == nil {
			t.Fatalf("%s lost JSON size limit", language)
		}
	}
}
