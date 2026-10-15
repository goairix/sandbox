package main

// freshControlMode receives the original stat mode, including its file type and
// special bits. It never masks an unapproved permission into an accepted one.
func freshControlMode(mode uint32) bool {
	return mode == 0040000|0700 || mode == 0040000|0755 || mode == 0040000|0777 || mode == 0040000|01777
}
