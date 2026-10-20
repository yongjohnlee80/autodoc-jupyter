package kernel

// What the external tests read of a client's internals.

// ConnFileOf is c's connection file.
func ConnFileOf(c *Client) string { return c.cur().connFile }

// PidOf is c's kernel process.
func PidOf(c *Client) int { return c.cur().cmd.Process.Pid }

// ExitedOf is closed once c's kernel process has exited.
func ExitedOf(c *Client) <-chan struct{} { return c.cur().exited }

// ArgvOf is s's command for the connection file at conn.
func ArgvOf(s Spec, conn string) []string { return s.argv(conn) }
