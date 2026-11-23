// Package xray holds what every reader of the 3x-ui Xray shares; its
// subpackages run, read and account for that Xray.
package xray

import "regexp"

var ownedUser = regexp.MustCompile(`^ak-[a-z0-9]{1,61}$`)

// Owned reports whether an Xray user, the email of a 3x-ui client, is one
// Cloud owns.
func Owned(email string) bool { return ownedUser.MatchString(email) }
