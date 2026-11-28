package realname

import "context"

// osFullName is the account's RealName from Directory Services.
func osFullName(ctx context.Context, login string) string {
	if login == "" {
		return ""
	}
	return run(ctx, "id", "-F", login)
}
