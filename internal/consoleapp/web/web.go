// Package web is the local console's page: plain HTML forms, readable on a phone,
// with nothing loaded from elsewhere (the hub's network has no internet). The texts
// are in French, for the installers; they are in the templates, not here.
//
// It serves only the hub's address on its own network, and only while the
// installation mode lasts (the program starts and stops it).
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"safehome/internal/consoleapp"
)

// Console is what the page needs from the console (consoleapp.App).
type Console interface {
	Overview() consoleapp.Overview
	Pair() error
	SetZone(id, name, kind string) error
	Unzone(id string) error
	Siren(id string, on bool) error
	Remove(ctx context.Context, id string) error
	SetCode(user, code, again string) error
	Finish(ctx context.Context) error
}

//go:embed templates
var files embed.FS

// Handler serves the page.
type Handler struct {
	c     Console
	host  string // the only Host accepted: the hub's address on its network
	loc   *time.Location
	pages *template.Template
	mux   *http.ServeMux
	csrf  *http.CrossOriginProtection
}

// New builds the page for the console, answering only requests made to host (the
// hub's address, with its port unless 80): a page reached under another name is
// refused (DNS rebinding).
func New(c Console, host string, loc *time.Location) (*Handler, error) {
	if loc == nil {
		loc = time.UTC
	}
	h := &Handler{c: c, host: host, loc: loc, mux: http.NewServeMux(), csrf: http.NewCrossOriginProtection()}
	funcs := template.FuncMap{
		"clock": func(t time.Time) string { return t.In(h.loc).Format("15:04:05") },
		"hour":  func(t time.Time) string { return t.In(h.loc).Format("15:04") },
		"join":  func(s []string) string { return strings.Join(s, ", ") },
	}
	pages, err := template.New("").Funcs(funcs).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h.pages = pages
	h.mux.HandleFunc("GET /{$}", h.home)
	h.mux.HandleFunc("GET /tests", h.tests)
	h.mux.HandleFunc("POST /pair", h.act("pair", func(r *http.Request, _ string) error { return h.c.Pair() }))
	h.mux.HandleFunc("POST /zone", h.act("zone", func(r *http.Request, id string) error {
		return h.c.SetZone(id, strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("kind"))
	}))
	h.mux.HandleFunc("POST /unzone", h.act("unzone", func(r *http.Request, id string) error { return h.c.Unzone(id) }))
	h.mux.HandleFunc("POST /siren", h.act("siren", func(r *http.Request, id string) error {
		return h.c.Siren(id, r.PostFormValue("on") == "1")
	}))
	h.mux.HandleFunc("POST /remove", h.act("remove", func(r *http.Request, id string) error { return h.c.Remove(r.Context(), id) }))
	// The code goes no further than the console: never in the answer, nor the address.
	h.mux.HandleFunc("POST /code", h.act("code", func(r *http.Request, user string) error {
		return h.c.SetCode(user, r.PostFormValue("code"), r.PostFormValue("again"))
	}))
	h.mux.HandleFunc("POST /finish", h.act("finish", func(r *http.Request, _ string) error { return h.c.Finish(r.Context()) }))
	return h, nil
}

// ServeHTTP checks the request's host and origin, and sets the headers of every
// answer. An order posted from another site's page is refused: the installer's
// phone may browse the internet over its mobile data while on the hub's network.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != h.host {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	if err := h.csrf.Check(r); err != nil {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	hd := w.Header()
	hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	h.mux.ServeHTTP(w, r)
}

// Outcome is what the last order gave: the templates say it in French from Code.
type Outcome struct {
	OK     bool
	Code   string // the order when OK ("zone"...), the refusal otherwise ("busy"...)
	ID     string // the device, if any
	On     bool   // for "siren": made one, or stopped
	Detail string // an unknown refusal's own text, for the installer to pass on
}

type page struct {
	Overview consoleapp.Overview
	Outcome  *Outcome
	Refresh  int // seconds, 0 for none
}

func (h *Handler) render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.pages.ExecuteTemplate(w, name, p); err != nil {
		http.Error(w, "page error", http.StatusInternalServerError)
	}
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	o := h.c.Overview()
	p := page{Overview: o}
	q := r.URL.Query()
	if code := q.Get("ok"); code != "" {
		p.Outcome = &Outcome{OK: true, Code: code, ID: q.Get("id"), On: q.Get("on") == "1"}
	} else if code := q.Get("err"); code != "" {
		p.Outcome = &Outcome{Code: code, ID: q.Get("id"), Detail: q.Get("detail")}
	}
	if o.Pairing != nil && o.Pairing.Active {
		p.Refresh = 3 // the devices appear as they join
	}
	h.render(w, "home.html", p)
}

func (h *Handler) tests(w http.ResponseWriter, r *http.Request) {
	h.render(w, "tests.html", page{Overview: h.c.Overview(), Refresh: 2})
}

// act runs a form's order, then shows the page again with what happened.
func (h *Handler) act(code string, do func(r *http.Request, id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		id := r.PostFormValue("id")
		q := url.Values{}
		if id != "" {
			q.Set("id", id)
		}
		if err := do(r, id); err != nil {
			refusal, detail := Refusal(err)
			q.Set("err", refusal)
			if detail != "" {
				q.Set("detail", detail)
			}
		} else {
			q.Set("ok", code)
			if r.PostFormValue("on") == "1" {
				q.Set("on", "1")
			}
		}
		http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
	}
}

// refusals are the console's known refusals, by the code the templates know them by.
var refusals = []struct {
	err  error
	code string
}{
	{consoleapp.ErrClosed, "closed"},
	{consoleapp.ErrBusy, "busy"},
	{consoleapp.ErrUnknown, "unknown"},
	{consoleapp.ErrNotASiren, "not_siren"},
	{consoleapp.ErrNoZone, "no_zone"},
	{consoleapp.ErrNoRegistry, "no_registry"},
	{consoleapp.ErrLastSiren, "last_siren"},
	{consoleapp.ErrEmptyName, "empty_name"},
	{consoleapp.ErrNameTooLong, "long_name"},
	{consoleapp.ErrCannotServe, "cannot_serve"},
	{consoleapp.ErrNoAnswer, "no_answer"},
	{consoleapp.ErrNotAContact, "not_contact"},
	{consoleapp.ErrCodeMismatch, "code_mismatch"},
	{consoleapp.ErrCodeInvalid, "code_invalid"},
	{consoleapp.ErrCodeWeak, "code_weak"},
	{consoleapp.ErrCodeTaken, "code_taken"},
	{consoleapp.ErrCodeFull, "code_full"},
}

// Refusal names a known refusal for the templates; an unknown one is "other", with
// its own text.
func Refusal(err error) (code, detail string) {
	for _, r := range refusals {
		if errors.Is(err, r.err) {
			return r.code, ""
		}
	}
	return "other", err.Error()
}
