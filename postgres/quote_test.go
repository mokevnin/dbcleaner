package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuote(t *testing.T) {
	require.Equal(t, `"order"`, quoteIdent("order"))
	require.Equal(t, `"we""ird"`, quoteIdent(`we"ird`))
}
