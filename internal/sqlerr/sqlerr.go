// Package sqlerr reads server error codes out of database driver errors
// without importing the drivers, so depending on job-queue doesn't pull in
// (or register) a driver the application doesn't use.
package sqlerr

import (
	"errors"
	"reflect"
)

// MySQLNumber returns the server error number of the first go-sql-driver/mysql
// *MySQLError in err's chain. The driver exposes it only as the struct's
// Number field, so the error is recognized by its type name and that field.
func MySQLNumber(err error) (uint16, bool) {
	var number uint16
	found := find(err, func(e error) bool {
		v := reflect.ValueOf(e)
		if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct || v.Elem().Type().Name() != "MySQLError" {
			return false
		}
		f := v.Elem().FieldByName("Number")
		if f.Kind() != reflect.Uint16 {
			return false
		}
		number = uint16(f.Uint())
		return true
	})
	return number, found
}

// SQLState returns the SQLSTATE of the first error in err's chain that has a
// SQLState() string method, such as pgx's *pgconn.PgError (used by
// gorm.io/driver/postgres).
func SQLState(err error) (string, bool) {
	var e interface{ SQLState() string }
	if !errors.As(err, &e) {
		return "", false
	}
	return e.SQLState(), true
}

// find reports whether match is true for err or any error it wraps, following
// Unwrap the way errors.As does.
func find(err error, match func(error) bool) bool {
	for err != nil {
		if match(err) {
			return true
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				if find(e, match) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}
