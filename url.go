package dbcleaner

import "context"

// URL is the URL of the database the Cleaner works on: the one it was built with, or the one
// the dialect leased for this process (postgres.WithTemplate). It starts what is needed (the
// lease, the setup) if that has not happened yet. Hand it to code that opens its own
// connection.
func (c *Cleaner) URL(ctx context.Context) (string, error) {
	if err := c.ready(ctx, ""); err != nil {
		return "", err
	}
	return c.url, nil
}
