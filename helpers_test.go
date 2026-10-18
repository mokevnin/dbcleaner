package pgcleaner_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// replaceDatabase points a server URL at another database on it.
func replaceDatabase(t *testing.T, serverURL, name string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}
