package main

// Authorizer manages user authorization via an allowlist.
type Authorizer struct {
	allowed map[string]bool
}

// NewAuthorizer creates a new Authorizer with the given allowed user IDs.
func NewAuthorizer(allowedUsers []string) *Authorizer {
	allowed := make(map[string]bool, len(allowedUsers))
	for _, userID := range allowedUsers {
		allowed[userID] = true
	}
	return &Authorizer{allowed: allowed}
}

// IsAuthorized returns true if the user ID is in the allowlist.
// If the allowlist is empty, all users are denied.
func (a *Authorizer) IsAuthorized(userID string) bool {
	if len(a.allowed) == 0 {
		return false
	}
	return a.allowed[userID]
}

// RejectMessage returns a friendly message for unauthorized users.
func (a *Authorizer) RejectMessage() string {
	return "Sorry, you're not authorized to use this bot. Please contact an administrator if you need access."
}
