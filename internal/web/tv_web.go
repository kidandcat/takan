package web

import (
	"net/http"

	tvmod "github.com/kidandcat/takan/modules/tv"
)

func (s *Server) saveTV(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/dashboard/tv?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	cfg := tvmod.ParseConfig("")
	if v := r.FormValue("machine"); v != "" {
		cfg.Machine = v
	}
	if v := r.FormValue("host"); v != "" {
		cfg.Host = v
	}
	if v := r.FormValue("token_path"); v != "" {
		cfg.TokenPath = v
	}
	if v := r.FormValue("client_name"); v != "" {
		cfg.ClientName = v
	}
	if raw := r.FormValue("apps"); raw != "" {
		apps, err := tvmod.ParseAppsText(raw)
		if err != nil {
			http.Redirect(w, r, "/dashboard/tv?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
			return
		}
		cfg.Apps = apps
	}
	if err := tvmod.SaveConfig(r.Context(), s.Store, u.ID, cfg); err != nil {
		http.Redirect(w, r, "/dashboard/tv?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "tv", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/tv?flash="+urlQuery("TV settings saved"), http.StatusFound)
}
