package sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuote(t *testing.T) {
	require.Equal(t, `"order"`, quote("order"))
	require.Equal(t, `"we""ird"`, quote(`we"ird`))
}
