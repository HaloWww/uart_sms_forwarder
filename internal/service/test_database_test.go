package service

import "github.com/google/uuid"

func uniqueTestSQLiteDSN(name string) string {
	return "file:" + name + "-" + uuid.NewString() + "?mode=memory&cache=shared"
}
