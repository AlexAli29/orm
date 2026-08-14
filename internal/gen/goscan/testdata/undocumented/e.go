package undocumented

type helper struct{ n int }

type Alias = helper

//orm:table widgets
type Widget struct {
	ID   int64
	Name string
}
