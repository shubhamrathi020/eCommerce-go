package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SavedAddress is a row of saved_addresses.
type SavedAddress struct {
	ID        string
	UserID    string
	Label     string
	Name      string
	Phone     string
	Line1     string
	Line2     *string
	City      string
	State     string
	Pincode   string
	IsDefault bool
	CreatedAt time.Time
}

const addrCols = `"id","userId","label","name","phone","line1","line2","city","state","pincode","isDefault","createdAt"`

func scanAddresses(rows pgx.Rows) ([]SavedAddress, error) {
	defer rows.Close()
	out := []SavedAddress{}
	for rows.Next() {
		var a SavedAddress
		if err := rows.Scan(&a.ID, &a.UserID, &a.Label, &a.Name, &a.Phone, &a.Line1, &a.Line2, &a.City, &a.State, &a.Pincode, &a.IsDefault, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddressesFor lists a user's addresses oldest first.
func AddressesFor(ctx context.Context, q DBTX, userID string) ([]SavedAddress, error) {
	rows, err := q.Query(ctx, `SELECT `+addrCols+` FROM saved_addresses WHERE "userId"=$1 ORDER BY "createdAt" ASC, "id" ASC`, userID)
	if err != nil {
		return nil, err
	}
	return scanAddresses(rows)
}

// CountAddresses counts a user's saved addresses.
func CountAddresses(ctx context.Context, q DBTX, userID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM saved_addresses WHERE "userId"=$1`, userID).Scan(&n)
	return n, err
}

// AddressFields are the editable parts of an address.
type AddressFields struct {
	Label, Name, Phone, Line1 string
	Line2                     *string
	City, State, Pincode      string
}

// CreateAddress inserts an address.
func CreateAddress(ctx context.Context, q DBTX, userID string, f AddressFields, isDefault bool) error {
	_, err := q.Exec(ctx, `INSERT INTO saved_addresses ("id","userId","label","name","phone","line1","line2","city","state","pincode","isDefault","createdAt") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		NewID(), userID, f.Label, f.Name, f.Phone, f.Line1, f.Line2, f.City, f.State, f.Pincode, isDefault, Now())
	return err
}

// UpdateAddress changes an address the user owns; false when there was none.
func UpdateAddress(ctx context.Context, q DBTX, id, userID string, f AddressFields) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE saved_addresses SET "label"=$3,"name"=$4,"phone"=$5,"line1"=$6,"line2"=$7,"city"=$8,"state"=$9,"pincode"=$10 WHERE "id"=$1 AND "userId"=$2`,
		id, userID, f.Label, f.Name, f.Phone, f.Line1, f.Line2, f.City, f.State, f.Pincode)
	return tag.RowsAffected() > 0, err
}

// AddressOf finds an address by id for its owner; nil when none.
func AddressOf(ctx context.Context, q DBTX, id, userID string) (*SavedAddress, error) {
	rows, err := q.Query(ctx, `SELECT `+addrCols+` FROM saved_addresses WHERE "id"=$1 AND "userId"=$2 LIMIT 1`, id, userID)
	if err != nil {
		return nil, err
	}
	list, err := scanAddresses(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// OldestAddress is the user's oldest saved address; nil when they have none.
func OldestAddress(ctx context.Context, q DBTX, userID string) (*SavedAddress, error) {
	rows, err := q.Query(ctx, `SELECT `+addrCols+` FROM saved_addresses WHERE "userId"=$1 ORDER BY "createdAt" ASC, "id" ASC LIMIT 1`, userID)
	if err != nil {
		return nil, err
	}
	list, err := scanAddresses(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// DeleteAddress removes one address.
func DeleteAddress(ctx context.Context, q DBTX, id string) error {
	_, err := q.Exec(ctx, `DELETE FROM saved_addresses WHERE "id"=$1`, id)
	return err
}

// ClearDefaults unsets the default on all of a user's addresses.
func ClearDefaults(ctx context.Context, q DBTX, userID string) error {
	_, err := q.Exec(ctx, `UPDATE saved_addresses SET "isDefault"=false WHERE "userId"=$1`, userID)
	return err
}

// SetDefaultAddress marks one address as the default.
func SetDefaultAddress(ctx context.Context, q DBTX, id string) error {
	_, err := q.Exec(ctx, `UPDATE saved_addresses SET "isDefault"=true WHERE "id"=$1`, id)
	return err
}

var _ = errors.Is
