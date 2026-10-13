package ticket

// RefError is refError for the external test package: the foreign-key backstop is mapped
// by constraint name and a real violation is too racy to provoke on demand.
var RefError = refError
