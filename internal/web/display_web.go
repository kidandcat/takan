package web

import (
	"net/http"
)

func (s *Server) createDisplay(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	name := r.FormValue("name")
	machineID := r.FormValue("machine_id")
	isDefault := r.FormValue("is_default") == "1"
	_, err := s.Store.CreateDisplay(r.Context(), u.ID, name, machineID, isDefault)
	if err != nil {
		http.Redirect(w, r, "/dashboard/display?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "display", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/display?flash="+urlQuery("Screen added"), http.StatusFound)
}

func (s *Server) deleteDisplay(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.DeleteDisplay(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Redirect(w, r, "/dashboard/display?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/display", http.StatusFound)
}

func (s *Server) defaultDisplay(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.SetDisplayDefault(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Redirect(w, r, "/dashboard/display?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/display?flash="+urlQuery("Default screen updated"), http.StatusFound)
}
