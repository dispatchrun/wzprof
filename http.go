package wazeroprofiler

import (
	"fmt"
	"net/http"
)

func (p *ProfilerListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff") //Took from go pprof

	pf := p.BuildProfile()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="profile"`)
	if err := pf.Write(w); err != nil {
		serveError(w, http.StatusInternalServerError, err.Error())
	}

	//TODO: clear cached data after collect
}

func serveError(w http.ResponseWriter, status int, txt string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Go-Pprof", "1")
	w.Header().Del("Content-Disposition")
	w.WriteHeader(status)
	fmt.Fprintln(w, txt)
}
