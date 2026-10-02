package model

import (
	"strings"
	"testing"
)

// Verification and reset codes must resolve an email or phone through
// network_user_auth_password first, where AddAuth puts added sign-ins, and
// only then through the legacy network_user.user_auth column.
func TestUserIdByUserAuthSqlPrefersPasswordAuths(t *testing.T) {
	sql := userIdByUserAuthSql("$3")

	passwordIndex := strings.Index(sql, "FROM network_user_auth_password WHERE user_auth = $3")
	legacyIndex := strings.Index(sql, "FROM network_user WHERE user_auth = $3")
	if passwordIndex < 0 {
		t.Fatalf("lookup does not read network_user_auth_password: %s", sql)
	}
	if legacyIndex < 0 {
		t.Fatalf("lookup dropped the legacy network_user fallback: %s", sql)
	}
	if legacyIndex < passwordIndex {
		t.Fatalf("legacy network_user.user_auth must be the fallback: %s", sql)
	}
	if !strings.HasPrefix(strings.TrimSpace(sql), "COALESCE(") {
		t.Fatalf("lookup must be a single scalar value: %s", sql)
	}
}
