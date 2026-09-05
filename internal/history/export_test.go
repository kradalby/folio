package history

// SetGCThresholds lowers the repack heuristics so a test does not have to make
// thousands of commits to see one. It returns a function that puts them back.
func SetGCThresholds(loose, packs int) (restore func()) {
	oldLoose, oldPacks := gcLooseObjects, gcPacks
	gcLooseObjects, gcPacks = loose, packs
	return func() { gcLooseObjects, gcPacks = oldLoose, oldPacks }
}
