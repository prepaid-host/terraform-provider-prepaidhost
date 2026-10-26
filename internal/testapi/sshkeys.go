// Copyright (c) Prepaid-Host
// SPDX-License-Identifier: MPL-2.0

package testapi

import (
	"crypto/md5" //nolint:gosec // the API's fingerprint format (MD5 of the key), not a security use
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// ResourceSSHKey is the State.Resources type of SSH keys.
const ResourceSSHKey = "ssh_key"

// GET/POST /account/ssh-keys and DELETE /account/ssh-keys/{id} like the API.
func init() {
	register(func(s *Server) {
		s.Handle(http.MethodGet, "/account/ssh-keys", handleListSSHKeys)
		s.Handle(http.MethodPost, "/account/ssh-keys", handleCreateSSHKey)
		s.Handle(http.MethodDelete, "/account/ssh-keys/{id}", handleDeleteSSHKey)
	})
}

var sshKeyPrefixes = []string{"ssh-rsa", "ssh-ed25519", "ssh-dss", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521"}

// SSHFingerprint is the API's key fingerprint: the MD5 of the decoded key
// as colon-separated hex pairs ("" if the key has no decodable second field).
func SSHFingerprint(publicKey string) string {
	parts := strings.Fields(publicKey)
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	sum := md5.Sum(raw) //nolint:gosec // see import
	h := hex.EncodeToString(sum[:])
	pairs := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

func presentSSHKey(attrs map[string]any) map[string]any {
	return map[string]any{
		"id":                     attrs["id"],
		"name":                   attrs["name"],
		"fingerprint":            attrs["fingerprint"],
		"provision_on_reinstall": attrs["provision_on_reinstall"],
		"created_at":             attrs["created_at"],
	}
}

func handleListSSHKeys(st *State, _ *Request) Response {
	keys := []any{}
	for _, id := range st.ResourceIDs(ResourceSSHKey) {
		attrs, _ := st.Resource(ResourceSSHKey, id)
		keys = append(keys, presentSSHKey(attrs))
	}
	return OK(keys)
}

func handleCreateSSHKey(st *State, r *Request) Response {
	body := r.BodyMap()
	name, _ := body["name"].(string)
	publicKey, _ := body["public_key"].(string)
	publicKey = strings.TrimSpace(publicKey)

	// The API's general validation answer (VALIDATION_ERROR with details).
	details := map[string]any{}
	if strings.TrimSpace(name) == "" {
		details["name"] = []any{"The name field is required."}
	} else if len([]rune(name)) > 100 {
		details["name"] = []any{"The name field must not be greater than 100 characters."}
	}
	validKey := false
	for _, p := range sshKeyPrefixes {
		if strings.HasPrefix(publicKey, p) {
			validKey = true
		}
	}
	switch {
	case publicKey == "":
		details["public_key"] = []any{"The public key field is required."}
	case !validKey:
		details["public_key"] = []any{"Invalid SSH public key format."}
	}
	if len(details) > 0 {
		return Fail(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Validierungsfehler.", map[string]any{"details": details})
	}

	fp := SSHFingerprint(publicKey)
	for _, id := range st.ResourceIDs(ResourceSSHKey) {
		attrs, _ := st.Resource(ResourceSSHKey, id)
		if attrs["fingerprint"] == fp {
			return Fail(http.StatusConflict, "DUPLICATE_KEY", "This SSH key is already stored.", nil)
		}
	}
	provision, _ := body["provision_on_reinstall"].(bool)
	id := st.AddResource(ResourceSSHKey, map[string]any{
		"name":                   name,
		"public_key":             publicKey,
		"fingerprint":            fp,
		"provision_on_reinstall": provision,
		"created_at":             time.Now().UTC().Format(time.RFC3339),
	})
	attrs, _ := st.Resource(ResourceSSHKey, id)
	return Created(presentSSHKey(attrs))
}

func handleDeleteSSHKey(st *State, r *Request) Response {
	id, ok := r.ParamInt64("id")
	if !ok {
		return Fail(http.StatusNotFound, "NOT_FOUND", "Endpoint nicht gefunden.", nil)
	}
	if _, found := st.Resource(ResourceSSHKey, id); !found {
		// A missing key: 404 NOT_FOUND (the route exists).
		return Fail(http.StatusNotFound, "NOT_FOUND", "Endpoint nicht gefunden.", nil)
	}
	delete(st.Resources[ResourceSSHKey], id)
	return Response{Status: http.StatusOK, Body: map[string]any{"success": true}}
}

// SeedSSHKey stores a key as if it had been created outside Terraform.
func (s *Server) SeedSSHKey(name, publicKey string) int64 {
	return s.SeedResource(ResourceSSHKey, map[string]any{
		"name":                   name,
		"public_key":             publicKey,
		"fingerprint":            SSHFingerprint(publicKey),
		"provision_on_reinstall": false,
		"created_at":             time.Now().UTC().Format(time.RFC3339),
	})
}
