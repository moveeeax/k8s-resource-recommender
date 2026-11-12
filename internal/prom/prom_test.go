package prom

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryParsesVector(t *testing.T) {
	const body = `{
	  "status": "success",
	  "data": {
	    "resultType": "vector",
	    "result": [
	      {"metric": {"namespace":"prod","pod":"api-7d9f8b6c5d-aaaaa","container":"app"}, "value": [1758900000, "0.2031"]},
	      {"metric": {"namespace":"prod","pod":"api-7d9f8b6c5d-bbbbb","container":"app"}, "value": [1758900000, "0.4102"]}
	    ]
	  }
	}`

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", r.URL.Path)
		}
		// ParseForm consumes the body, so it must come before any read of it.
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotQuery = r.FormValue("query")
		if gotQuery == "" {
			t.Error("no query parameter in the request body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).Query(context.Background(), `up{job="x"}`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotQuery != `up{job="x"}` {
		t.Errorf("server saw query %q, want the expression verbatim", gotQuery)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[0].Value != 0.2031 {
		t.Errorf("sample 0 value = %v, want 0.2031", got[0].Value)
	}
	if got[0].Labels["pod"] != "api-7d9f8b6c5d-aaaaa" {
		t.Errorf("sample 0 pod = %q", got[0].Labels["pod"])
	}
}

// Prometheus signals a NaN for series with no data in the window. That is not
// an error condition, those series just carry nothing to size against.
func TestQuerySkipsNaNAndInf(t *testing.T) {
	const body = `{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"pod":"a","container":"c"},"value":[1,"NaN"]},
	  {"metric":{"pod":"b","container":"c"},"value":[1,"+Inf"]},
	  {"metric":{"pod":"c","container":"c"},"value":[1,"1.5"]}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).Query(context.Background(), "x")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].Value != 1.5 {
		t.Fatalf("got %+v, want only the 1.5 sample", got)
	}
}

// A PromQL error comes back as HTTP 400 with a JSON body; the message in that
// body is the useful part, so it must survive into the returned error.
func TestQuerySurfacesPromQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"parse error: unexpected ]"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, 5*time.Second).Query(context.Background(), "bad]")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "parse error: unexpected ]") {
		t.Errorf("error = %v, want it to include the Prometheus message", err)
	}
}

func TestQueryRejectsNonVectorResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 5*time.Second).Query(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a matrix result")
	} else if !strings.Contains(err.Error(), "matrix") {
		t.Errorf("error = %v, want it to name the result type", err)
	}
}

func TestQueryHandlesNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	}))
	defer srv.Close()

	_, err := New(srv.URL, 5*time.Second).Query(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want it to mention the HTTP status", err)
	}
}

func TestQueryUnreachableEndpoint(t *testing.T) {
	// Port 0 on a closed listener: connection refused without a real server.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := New(url, 500*time.Millisecond).Query(context.Background(), "x")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if !strings.Contains(err.Error(), "query prometheus at") {
		t.Errorf("error = %v, want it to name the endpoint", err)
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	if got := New("http://prom:9090/", time.Second).BaseURL; got != "http://prom:9090" {
		t.Errorf("BaseURL = %q, want the slash trimmed", got)
	}
}

// QueryRaw hands back the body untouched, and Parse reads it the same way
// Query does: that pair is what makes --save-queries / --from-dir replayable.
func TestQueryRawRoundTrip(t *testing.T) {
	const body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":"a"},"value":[1,"0.5"]}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	raw, err := New(srv.URL, 5*time.Second).QueryRaw(context.Background(), "x")
	if err != nil {
		t.Fatalf("QueryRaw: %v", err)
	}
	if string(raw) != body {
		t.Errorf("raw = %q, want the body verbatim", raw)
	}
	got, err := Parse(raw)
	if err != nil || len(got) != 1 || got[0].Value != 0.5 {
		t.Errorf("Parse = %+v, %v", got, err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("err = %v, want a decode error", err)
	}
	if _, err := Parse([]byte(`{"status":"error","errorType":"bad_data","error":"boom"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the Prometheus message", err)
	}
}
