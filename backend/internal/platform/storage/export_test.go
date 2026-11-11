package storage

// IsSpecificDeclaration is exported for tests so the declared-type logic can be
// exercised directly rather than only through a full upload against live storage.
func IsSpecificDeclaration(declared string) bool { return isSpecificDeclaration(declared) }
