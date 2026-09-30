// Package kubefake is an in-memory fake Kubernetes API server for tests.
// It implements only the calls the agent uses, with the same error responses
// as the real API (404, 403, 429 for PodDisruptionBudgets).
package kubefake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/thumbops/agent/internal/kube"
)

type Server struct {
	mu          sync.Mutex
	deployments map[string]*kube.Deployment
	nodes       map[string]*kube.Node
	pods        map[string]*kube.Pod
	namespaces  map[string]string // name → UID
	denied      map[string]bool   // e.g. "patch apps/deployments"
	evictBlocks map[string]int    // "ns/pod" → how many 429s to return; -1 = always
	patches     []string
	evictions   []string
	version     string
	srv         *httptest.Server
}

func New() *Server {
	s := &Server{
		deployments: map[string]*kube.Deployment{},
		nodes:       map[string]*kube.Node{},
		pods:        map[string]*kube.Pod{},
		namespaces:  map[string]string{"kube-system": "b3b0c1c2-0000-4000-8000-000000000001"},
		denied:      map[string]bool{},
		evictBlocks: map[string]int{},
		version:     "v1.34.3",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", s.getVersion)
	mux.HandleFunc("GET /api/v1/nodes", s.listNodes)
	mux.HandleFunc("GET /api/v1/nodes/{name}", s.getNode)
	mux.HandleFunc("PATCH /api/v1/nodes/{name}", s.patchNode)
	mux.HandleFunc("GET /api/v1/pods", s.listPods)
	mux.HandleFunc("GET /api/v1/namespaces/{name}", s.getNamespace)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{name}", s.getPod)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/pods/{name}/eviction", s.evict)
	mux.HandleFunc("GET /apis/apps/v1/namespaces/{ns}/deployments/{name}", s.getDeployment)
	mux.HandleFunc("PATCH /apis/apps/v1/namespaces/{ns}/deployments/{name}", s.patchDeployment)
	mux.HandleFunc("POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews", s.accessReview)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *Server) URL() string { return s.srv.URL }
func (s *Server) Close()      { s.srv.Close() }

// Client returns an agent client connected to this server.
func (s *Server) Client() *kube.Client {
	c, err := kube.New(kube.Config{Host: s.srv.URL})
	if err != nil {
		panic(err)
	}
	return c
}

// --- state setup ---

func (s *Server) AddDeployment(ns, name string, replicas int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := replicas
	s.deployments[ns+"/"+name] = &kube.Deployment{
		Metadata: kube.ObjectMeta{Name: name, Namespace: ns, UID: "uid-" + name},
		Spec:     kube.DeploymentSpec{Replicas: &r, Template: &kube.PodTemplateSpec{}},
	}
}

func (s *Server) AddNode(name string, ready bool, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := "False"
	if ready {
		st = "True"
	}
	s.nodes[name] = &kube.Node{
		Metadata: kube.ObjectMeta{Name: name, UID: "uid-" + name, Labels: labels},
		Status:   kube.NodeStatus{Conditions: []kube.NodeCondition{{Type: "Ready", Status: st}}},
	}
}

func (s *Server) AddPod(p kube.Pod) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Metadata.UID == "" {
		p.Metadata.UID = "uid-" + p.Metadata.Name
	}
	s.pods[p.Metadata.Namespace+"/"+p.Metadata.Name] = &p
}

// Deny makes a call respond 403, e.g. Deny("patch", "apps", "deployments", "").
func (s *Server) Deny(verb, group, resource, sub string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied[permKey(verb, group, resource, sub)] = true
}

// BlockEviction simulates a PodDisruptionBudget: n responses 429, -1 forever.
func (s *Server) BlockEviction(ns, pod string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictBlocks[ns+"/"+pod] = n
}

// --- state inspection ---

func (s *Server) Deployment(ns, name string) kube.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(*s.deployments[ns+"/"+name])
}

func (s *Server) Node(name string) kube.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(*s.nodes[name])
}

func (s *Server) PodExists(ns, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pods[ns+"/"+name]
	return ok
}

func (s *Server) Patches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.patches...)
}

func (s *Server) Evictions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.evictions...)
}

// --- handler ---

func permKey(verb, group, resource, sub string) string {
	r := resource
	if sub != "" {
		r += "/" + sub
	}
	if group != "" {
		r = group + "/" + r
	}
	return verb + " " + r
}

// allowed must be called with the lock held.
func (s *Server) allowed(w http.ResponseWriter, verb, group, resource, sub string) bool {
	if s.denied[permKey(verb, group, resource, sub)] {
		writeStatus(w, http.StatusForbidden, "Forbidden", fmt.Sprintf("%s is forbidden", permKey(verb, group, resource, sub)))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	writeJSON(w, code, map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "message": msg, "code": code,
	})
}

func notFound(w http.ResponseWriter, what string) {
	writeStatus(w, http.StatusNotFound, "NotFound", what+" not found")
}

func (s *Server) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"gitVersion": s.version})
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "list", "", "nodes", "") {
		return
	}
	items := []kube.Node{}
	for _, n := range s.nodes {
		items = append(items, *n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "get", "", "nodes", "") {
		return
	}
	n, ok := s.nodes[r.PathValue("name")]
	if !ok {
		notFound(w, "nodes "+r.PathValue("name"))
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) patchNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "patch", "", "nodes", "") {
		return
	}
	name := r.PathValue("name")
	n, ok := s.nodes[name]
	if !ok {
		notFound(w, "nodes "+name)
		return
	}
	var out kube.Node
	if !applyPatch(w, r, n, &out) {
		return
	}
	s.nodes[name] = &out
	s.patches = append(s.patches, "node/"+name)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listPods(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "list", "", "pods", "") {
		return
	}
	node := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "spec.nodeName=")
	items := []kube.Pod{}
	for _, p := range s.pods {
		if node == "" || p.Spec.NodeName == node {
			items = append(items, *p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getNamespace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.namespaces[r.PathValue("name")]
	if !ok {
		notFound(w, "namespaces "+r.PathValue("name"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]string{"name": r.PathValue("name"), "uid": uid}})
}

func (s *Server) getPod(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "get", "", "pods", "") {
		return
	}
	p, ok := s.pods[r.PathValue("ns")+"/"+r.PathValue("name")]
	if !ok {
		notFound(w, "pods "+r.PathValue("name"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) evict(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "create", "", "pods", "eviction") {
		return
	}
	key := r.PathValue("ns") + "/" + r.PathValue("name")
	if _, ok := s.pods[key]; !ok {
		notFound(w, "pods "+r.PathValue("name"))
		return
	}
	if n := s.evictBlocks[key]; n != 0 {
		if n > 0 {
			s.evictBlocks[key] = n - 1
		}
		writeStatus(w, http.StatusTooManyRequests, "TooManyRequests",
			"Cannot evict pod as it would violate the pod's disruption budget.")
		return
	}
	delete(s.pods, key)
	s.evictions = append(s.evictions, key)
	writeJSON(w, http.StatusCreated, map[string]any{"kind": "Status", "status": "Success"})
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "get", "apps", "deployments", "") {
		return
	}
	d, ok := s.deployments[r.PathValue("ns")+"/"+r.PathValue("name")]
	if !ok {
		notFound(w, "deployments.apps "+r.PathValue("name"))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) patchDeployment(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(w, "patch", "apps", "deployments", "") {
		return
	}
	key := r.PathValue("ns") + "/" + r.PathValue("name")
	d, ok := s.deployments[key]
	if !ok {
		notFound(w, "deployments.apps "+r.PathValue("name"))
		return
	}
	var out kube.Deployment
	if !applyPatch(w, r, d, &out) {
		return
	}
	s.deployments[key] = &out
	s.patches = append(s.patches, "deployment/"+key)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) accessReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Spec struct {
			ResourceAttributes kube.ResourceAttributes `json:"resourceAttributes"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	a := req.Spec.ResourceAttributes
	s.mu.Lock()
	allowed := !s.denied[permKey(a.Verb, a.Group, a.Resource, a.Subresource)]
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"status": map[string]bool{"allowed": allowed},
	})
}

// applyPatch applies a JSON merge patch (RFC 7386) as the API server does.
func applyPatch(w http.ResponseWriter, r *http.Request, cur, out any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
		writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "content type "+ct)
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return false
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "invalid patch: "+err.Error())
		return false
	}
	b, _ := json.Marshal(cur)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	merged, _ := json.Marshal(mergePatch(doc, patch))
	if err := json.Unmarshal(merged, out); err != nil {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return false
	}
	return true
}

func mergePatch(dst, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			dv, _ := dst[k].(map[string]any)
			dst[k] = mergePatch(dv, pv)
		default:
			dst[k] = v
		}
	}
	return dst
}

func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}

// PatchNodeForTest applies a JSON merge patch to a node, as a test setup
// step (no permission check, not recorded in Patches).
func (s *Server) PatchNodeForTest(name string, patch map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[name]
	cur := map[string]any{}
	b, _ := json.Marshal(n)
	_ = json.Unmarshal(b, &cur)
	merged := mergePatch(cur, patch)
	b, _ = json.Marshal(merged)
	var out kube.Node
	_ = json.Unmarshal(b, &out)
	s.nodes[name] = &out
}
