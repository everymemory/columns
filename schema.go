package keine

// ColumnSchema describes one column of a file.
type ColumnSchema struct {
	Name     string
	Type     uint8
	Nullable bool
}
