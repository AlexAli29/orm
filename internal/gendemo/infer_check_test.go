package gendemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// The write options take a typed column interface whose entity parameter
// appears in a method signature. That is what lets Go infer E from the
// arguments, so callers write Default(Users.CreatedAt) rather than
// Default[User](Users.CreatedAt). If inference ever stopped working this would
// fail to compile.
func TestTypeInferenceOnWriteOptions(t *testing.T) {
	_ = orm.Default(gendemo.Users.CreatedAt, gendemo.Users.Nickname)
	_ = orm.OnConflict(gendemo.Users.Email).DoNothing()
	_ = orm.OnConflict(gendemo.Users.Email).
		DoUpdate(gendemo.Users.Nickname, gendemo.Users.Age)
	_ = gendemo.Users.Age.Set(1)
	_ = gendemo.Users.Nickname.SetNull()
}
