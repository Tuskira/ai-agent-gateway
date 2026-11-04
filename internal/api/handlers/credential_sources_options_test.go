package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
)

// optionsService is a credential service for the options route: it
// records the last request and answers with whatever body/status the test
// sets. It accepts the bearer token "gw-token".
type optionsService struct {
	srv *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	last   url.Values
}

func (o *optionsService) set(status int, body string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status, o.body = status, body
}

func (o *optionsService) lastQuery() url.Values {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

func newOptionsDeps(t *testing.T) (Deps, *optionsService) {
	t.Helper()
	d := newTestDeps()
	if err := d.Headers.RegisterExternal(secrets.NewExternalServiceProvider(d.Secrets.Fetcher(), 2*time.Second, time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.Headers.SetSourceLookup(dataplaneheaders.StoreSourceLookup(d.Store.CredentialSources()))

	svc := &optionsService{status: http.StatusOK, body: `{"options":[]}`}
	svc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gw-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		svc.mu.Lock()
		svc.last = r.URL.Query()
		status, body := svc.status, svc.body
		svc.mu.Unlock()
		if r.URL.Path != "/v1/credentials/options" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(svc.srv.Close)
	return d, svc
}

type optionsResult struct {
	Options []credentialSourceOption `json:"options"`
	Error   string                   `json:"error"`
}

func TestCredentialSources_Options(t *testing.T) {
	d, svc := newOptionsDeps(t)
	h := CredentialSources{Deps: d}
	ctx := context.Background()

	create := func(tenant, name string) credentialSourceView {
		var v credentialSourceView
		body := `{"name":"` + name + `","provider":"external_service","config":{"url":"` + svc.srv.URL + `","auth_credential":"vault-auth"}}`
		if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, tenant, body, &v); code != http.StatusCreated {
			t.Fatalf("create %s status = %d", name, code)
		}
		return v
	}
	src := create("tenant-a", "vault-a")
	if _, err := d.Secrets.Create(ctx, "tenant-a", "vault-auth", "generic", map[string]string{"token": "gw-token"}, "u"); err != nil {
		t.Fatal(err)
	}

	options := func(tenant, id, query string, dst *optionsResult) int {
		return sourceReq(t, http.MethodGet, "/credential-sources/{id}/options", "/credential-sources/"+id+"/options"+query, h.Options, tenant, "", dst)
	}

	t.Run("lists the service's options", func(t *testing.T) {
		svc.set(http.StatusOK, `{"options":[
			{"value":"splunk-prod","label":"Splunk (prod)"},
			{"value":"ticketing"},
			"bare-string",
			{"value":"splunk-prod","label":"duplicate"},
			{"value":"","label":"empty value"},
			42
		]}`)
		var res optionsResult
		if code := options("tenant-a", src.ID, "?field=credential_id", &res); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		want := []credentialSourceOption{
			{Value: "splunk-prod", Label: "Splunk (prod)"},
			{Value: "ticketing", Label: "ticketing"},
			{Value: "bare-string", Label: "bare-string"},
		}
		if res.Error != "" || len(res.Options) != len(want) {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		for i := range want {
			if res.Options[i] != want[i] {
				t.Errorf("option %d = %+v, want %+v", i, res.Options[i], want[i])
			}
		}
		// The service is asked for this tenant and field, never one the
		// caller picked.
		if q := svc.lastQuery(); q.Get("tenant_id") != "tenant-a" || q.Get("field") != "credential_id" {
			t.Errorf("service query = %v", q)
		}
	})

	t.Run("an empty list is options [] not null", func(t *testing.T) {
		svc.set(http.StatusOK, `{"options":[]}`)
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/credential-sources/"+src.ID+"/options?field=credential_id", nil), "tenant-a", "admin")
		w := serve(http.MethodGet, "/credential-sources/{id}/options", h.Options, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"options":[]`) {
			t.Errorf("status = %d, body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("field is required and must be one the provider lists", func(t *testing.T) {
		for name, query := range map[string]string{
			"missing":            "",
			"blank":              "?field=%20",
			"not a listed field": "?field=prefix",
		} {
			if code := options("tenant-a", src.ID, query, nil); code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", name, code)
			}
		}
	})

	t.Run("unknown source is 404, and so is another tenant's", func(t *testing.T) {
		if code := options("tenant-a", "no-such-id", "?field=credential_id", nil); code != http.StatusNotFound {
			t.Errorf("unknown source status = %d, want 404", code)
		}
		if code := options("tenant-b", src.ID, "?field=credential_id", nil); code != http.StatusNotFound {
			t.Errorf("other tenant status = %d, want 404", code)
		}
	})

	t.Run("a service failure is reported in the body", func(t *testing.T) {
		svc.set(http.StatusInternalServerError, `boom`)
		var res optionsResult
		if code := options("tenant-a", src.ID, "?field=credential_id", &res); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if !strings.Contains(res.Error, "500") || len(res.Options) != 0 {
			t.Errorf("result = %+v", res)
		}

		svc.set(http.StatusOK, `not json`)
		res = optionsResult{}
		options("tenant-a", src.ID, "?field=credential_id", &res)
		if !strings.Contains(res.Error, "not the expected JSON") || len(res.Options) != 0 {
			t.Errorf("bad body result = %+v", res)
		}
	})

	t.Run("a rejected or missing token is reported", func(t *testing.T) {
		if err := d.Secrets.Rotate(ctx, "tenant-a", "vault-auth", map[string]string{"token": "wrong"}); err != nil {
			t.Fatal(err)
		}
		var res optionsResult
		options("tenant-a", src.ID, "?field=credential_id", &res)
		if !strings.Contains(res.Error, "401") {
			t.Errorf("rejected token result = %+v", res)
		}
		if err := d.Secrets.Rotate(ctx, "tenant-a", "vault-auth", map[string]string{"token": "gw-token"}); err != nil {
			t.Fatal(err)
		}

		other := create("tenant-a", "no-token")
		if code := sourceReq(t, http.MethodPut, "/credential-sources/{id}", "/credential-sources/"+other.ID, h.Update, "tenant-a",
			`{"config":{"url":"`+svc.srv.URL+`","auth_credential":"missing-cred"}}`, nil); code != http.StatusOK {
			t.Fatalf("update status = %d", code)
		}
		res = optionsResult{}
		options("tenant-a", other.ID, "?field=credential_id", &res)
		if !strings.Contains(res.Error, "missing-cred") {
			t.Errorf("missing credential result = %+v", res)
		}
	})

	t.Run("a disabled source is not asked", func(t *testing.T) {
		off := create("tenant-a", "vault-off")
		if code := sourceReq(t, http.MethodPut, "/credential-sources/{id}", "/credential-sources/"+off.ID, h.Update, "tenant-a", `{"enabled":false}`, nil); code != http.StatusOK {
			t.Fatalf("disable status = %d", code)
		}
		svc.set(http.StatusOK, `{"options":[{"value":"x"}]}`)
		var res optionsResult
		options("tenant-a", off.ID, "?field=credential_id", &res)
		if !strings.Contains(res.Error, "disabled") || len(res.Options) != 0 {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestHeaders_List_NeedsSourceAndOptionFields(t *testing.T) {
	d, svc := newOptionsDeps(t)
	h := CredentialSources{Deps: d}
	var made credentialSourceView
	body := `{"name":"vault-a","provider":"external_service","config":{"url":"` + svc.srv.URL + `","auth_credential":"vault-auth"}}`
	if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, "tenant-a", body, &made); code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}
	var got providersResponse
	sourceReq(t, http.MethodGet, "/headers/providers", "/headers/providers", Headers{Deps: d}.List, "tenant-a", "", &got)
	for _, p := range got.Providers {
		switch p.ID {
		case "external_service":
			if !p.NeedsSource || strings.Join(p.OptionFields, ",") != "credential_id" {
				t.Errorf("external_service = needs_source %v, option_fields %v", p.NeedsSource, p.OptionFields)
			}
			// The ids are what the options route is addressed by.
			if len(p.SourceRefs) != 1 || p.SourceRefs[0].ID != made.ID || p.SourceRefs[0].Name != "vault-a" {
				t.Errorf("source_refs = %+v, want vault-a/%s", p.SourceRefs, made.ID)
			}
		default:
			if p.NeedsSource || len(p.OptionFields) != 0 || p.SourceRefs != nil {
				t.Errorf("%s gained source fields: %+v", p.ID, p)
			}
		}
	}
}
