package decisions

// SetHubLimit sets how many changes one request of Hub's feed of decisions asks, for a test; it returns the restore.
func SetHubLimit(n int) (restore func()) {
	old := hubLimit
	hubLimit = n
	return func() { hubLimit = old }
}
