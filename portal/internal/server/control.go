package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"portal/internal/apps"
	"portal/internal/docker"
)

// ---- Stop / start from the dashboard ----
//
// Two ways in:
//   - a container row: POST /api/containers/{id}/stop (and the existing
//     /restart, which also starts a stopped container);
//   - an app row, for apps that list their containers in apps.yaml:
//     POST /api/apps/{id}/stop and /api/apps/{id}/start, acting on exactly
//     those containers.
//
// Both refuse to stop the platform's own lifeline containers (see
// protectedServices) and silence/unsilence the matching alerts in
// Alertmanager (silences.go).

// protectedServices are the compose services of project "platform" that
// the dashboard itself depends on. Stopping any of them would lock you
// out of the page you would use to start it again: Portal (the page and
// the login), Caddy (the front door), CoreDNS (the names), and the
// socket proxies (Portal's and the backup's only way to Docker). They
// can still be restarted.
var protectedServices = map[string]bool{
	"portal": true, "caddy": true, "coredns": true, "docker-proxy": true, "docker-proxy-backup": true,
}

const platformProject = "platform"

// controlTimeout bounds one stop/start request. docker stop waits up to
// 10 s per container before killing it.
const controlTimeout = 60 * time.Second

// isProtected matches by compose labels, not by name, so a container
// that merely has "portal" in its name is not protected, and renaming a
// platform container does not unprotect it.
func isProtected(c docker.Container) bool {
	return c.Project == platformProject && protectedServices[c.Service]
}

// controlGuard is the common gate for every state-changing container
// request: a live session, then our own Origin (see sameOrigin).
func (s *Server) controlGuard(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.sessionToken(r); !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return false
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return false
	}
	return true
}

// containerByID returns the container with exactly this id from the
// collector's last list. Only IDs from our own list are accepted, so a
// URL sent to Docker is never built from arbitrary input.
func (s *Server) containerByID(id string) (docker.Container, bool) {
	if !docker.ValidID(id) {
		return docker.Container{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.containers {
		if c.ID == id {
			return c, true
		}
	}
	return docker.Container{}, false
}

// appsWith returns the ids of apps whose containers: list names this container.
func (s *Server) appsWith(name string) []string {
	var out []string
	for _, a := range s.apps.List() {
		for _, c := range a.Containers {
			if c == name {
				out = append(out, a.ID)
			}
		}
	}
	return out
}

// appContainers resolves an app's containers: list against the live
// container list. problem is a sentence for the dashboard when any name
// is unknown or protected; then nothing may be sent to Docker.
func appContainers(a apps.App, list []docker.Container) (found []docker.Container, problem string) {
	byName := make(map[string]docker.Container, len(list))
	for _, c := range list {
		byName[c.Name] = c
	}
	var unknown, protected []string
	for _, n := range a.Containers {
		c, ok := byName[n]
		switch {
		case !ok:
			unknown = append(unknown, n)
		case isProtected(c):
			protected = append(protected, n)
		default:
			found = append(found, c)
		}
	}
	switch {
	case len(unknown) > 0:
		return nil, "apps.yaml lists " + plural(len(unknown), "container", "containers") + " Docker does not have: " + strings.Join(unknown, ", ")
	case len(protected) > 0:
		return nil, "apps.yaml lists platform containers that cannot be stopped from here: " + strings.Join(protected, ", ")
	}
	return found, ""
}

// silenceKeys are the Alertmanager matchers for a set of containers and
// apps: name="<container>" (ContainerDown and every other per-container
// alert) and app="<id>" (AppDown).
func silenceKeys(names, appIDs []string) []silenceKey {
	var out []silenceKey
	for _, n := range names {
		out = append(out, silenceKey{"name", n})
	}
	for _, a := range appIDs {
		out = append(out, silenceKey{"app", a})
	}
	return out
}

const silenceWarning = "Alertmanager not reachable: alerts were not silenced. Expect a ContainerDown notification."

// silenceAll creates a silence per key. Failure never fails the stop: it
// is logged and returned as a warning for the page.
func (s *Server) silenceAll(ctx context.Context, keys []silenceKey, who string) string {
	if !s.silencer.enabled() {
		return ""
	}
	warn := ""
	for _, k := range keys {
		id, err := s.silencer.Silence(ctx, k, "stopped from Portal by "+who)
		if err != nil {
			log.Printf("silence %s: %v", k, err)
			warn = silenceWarning
			break // Alertmanager is likely down: do not wait once per key
		}
		log.Printf("silenced %s for %s (silence %s)", k, silenceDuration, id)
	}
	if warn == "" {
		s.refreshAlertsView()
	}
	return warn
}

// unsilenceAll expires Portal's silences for each key, best effort.
func (s *Server) unsilenceAll(ctx context.Context, keys []silenceKey) string {
	if !s.silencer.enabled() {
		return ""
	}
	warn := ""
	for _, k := range keys {
		n, err := s.silencer.Unsilence(ctx, k)
		if err != nil {
			log.Printf("unsilence %s: %v", k, err)
			warn = "Alertmanager not reachable: Portal's silence may still be active (it expires by itself after 30 days)."
			break
		}
		if n > 0 {
			log.Printf("ending %d silence(s) for %s in %s", n, k, silenceGrace)
		}
	}
	s.refreshAlertsView()
	return warn
}

// refreshAlertsView re-renders the status sentence, whose alert clause
// leaves out alerts Portal has silenced.
func (s *Server) refreshAlertsView() { s.publishStatus() }

// afterControl refreshes the container list and pushes the new state to
// every open dashboard (both tables: an app row's button depends on its
// containers).
func (s *Server) afterControl(ctx context.Context) {
	s.collectContainers(ctx)
	s.publishContainers()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleStop stops one container from the Containers table.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !s.controlGuard(w, r) {
		return
	}
	c, ok := s.containerByID(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown container", http.StatusNotFound)
		return
	}
	if isProtected(c) {
		log.Printf("stop %s refused (platform container) for %s from %s", c.Name, s.cfg.User, clientIP(r))
		http.Error(w, "this platform container cannot be stopped from the dashboard (it can be restarted)", http.StatusForbidden)
		return
	}
	log.Printf("stop %s requested by %s from %s", c.Name, s.cfg.User, clientIP(r))
	// Detached from the request: if the browser disconnects, the stop and
	// the silence still complete.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), controlTimeout)
	defer cancel()
	if err := s.docker.Stop(ctx, c.ID); err != nil {
		log.Printf("stop %s: %v", c.Name, err)
		http.Error(w, "Docker refused the stop", http.StatusBadGateway)
		return
	}
	warn := s.silenceAll(ctx, silenceKeys([]string{c.Name}, s.appsWith(c.Name)), s.cfg.User)
	s.afterControl(ctx)
	writeJSON(w, http.StatusOK, map[string]string{"stopped": c.Name, "warning": warn})
}

// handleAppControl stops or starts every container an app lists in
// apps.yaml. action is "stop" or "start".
func (s *Server) handleAppControl(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.controlGuard(w, r) {
			return
		}
		a, ok := s.apps.ByID(r.PathValue("id"))
		if !ok || len(a.Containers) == 0 {
			http.Error(w, "unknown app, or it lists no containers in apps.yaml", http.StatusNotFound)
			return
		}
		s.mu.RLock()
		list := s.containers
		s.mu.RUnlock()
		targets, problem := appContainers(a, list)
		if problem != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": problem})
			return
		}
		var names []string
		for _, c := range targets {
			names = append(names, c.Name)
		}
		log.Printf("%s app %s (%s) requested by %s from %s", action, a.ID, strings.Join(names, ", "), s.cfg.User, clientIP(r))
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), controlTimeout)
		defer cancel()

		var failed []string
		for _, c := range targets {
			var err error
			switch {
			case action == "stop" && c.Running():
				err = s.docker.Stop(ctx, c.ID)
			case action == "start" && !c.Running():
				// The proxy allows restart, not start; Docker's restart of
				// a stopped container simply starts it.
				err = s.docker.Restart(ctx, c.ID)
			default:
				continue // already in the wanted state
			}
			if err != nil {
				log.Printf("%s %s: %v", action, c.Name, err)
				failed = append(failed, c.Name)
			}
		}

		keys := silenceKeys(names, []string{a.ID})
		warn := ""
		if action == "stop" {
			// Silence even if one container failed to stop: the others did.
			warn = s.silenceAll(ctx, keys, s.cfg.User)
		} else if len(failed) == 0 {
			warn = s.unsilenceAll(ctx, keys)
		}
		s.afterControl(ctx)
		if len(failed) > 0 {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Docker refused to %s %s", action, strings.Join(failed, ", ")), "warning": warn})
			return
		}
		past := map[string]string{"stop": "stopped", "start": "started"}[action]
		writeJSON(w, http.StatusOK, map[string]any{"app": a.Name, past: names, "warning": warn})
	}
}

// silencedSubjects is the set of alert subjects (container names and app
// ids) Portal currently holds a silence for, so the status sentence does
// not shout about something stopped on purpose.
func (s *Server) silencedSubjects() map[string]bool {
	out := map[string]bool{}
	if !s.silencer.enabled() {
		return out
	}
	s.silencer.mu.Lock()
	defer s.silencer.mu.Unlock()
	for k := range s.silencer.ids {
		if _, v, ok := strings.Cut(k, "="); ok {
			out[v] = true
		}
	}
	return out
}
