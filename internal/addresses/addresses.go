// Package addresses is the signed-in customer's address book (at most ten addresses, one default).
package addresses

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

var (
	phoneRE   = regexp.MustCompile(`^[6-9][0-9]{9}$`)
	pincodeRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)
)

// MaxAddresses is how many a customer may save.
const MaxAddresses = 10

// ToSavedAddress maps a database row to the public contract.
func ToSavedAddress(r *db.SavedAddress) domain.SavedAddress {
	a := domain.Address{Line1: r.Line1, City: r.City, State: r.State, Pincode: r.Pincode}
	if r.Line2 != nil && *r.Line2 != "" {
		a.Line2 = *r.Line2
	}
	return domain.SavedAddress{ID: r.ID, Label: r.Label, Name: r.Name, Phone: r.Phone, Address: a, IsDefault: r.IsDefault}
}

// Input is a validated-for-type address form.
type Input struct {
	Label, Name, Phone                 string
	Line1, Line2, City, State, Pincode string
	HasLine2                           bool
}

// validate applies the same rules and messages as the mock address book the frontend was built against.
func validate(in Input) (db.AddressFields, error) {
	fields := map[string]string{}
	if l := strings.TrimSpace(in.Label); l == "" || len([]rune(l)) > 30 {
		fields["label"] = "Give this address a short label"
	}
	if strings.TrimSpace(in.Name) == "" {
		fields["name"] = "Name is required"
	}
	if !phoneRE.MatchString(in.Phone) {
		fields["phone"] = "Enter a valid 10-digit mobile number"
	}
	if strings.TrimSpace(in.Line1) == "" {
		fields["line1"] = "Address is required"
	}
	if strings.TrimSpace(in.City) == "" {
		fields["city"] = "City is required"
	}
	if strings.TrimSpace(in.State) == "" {
		fields["state"] = "State is required"
	}
	if !pincodeRE.MatchString(in.Pincode) {
		fields["pincode"] = "Enter a valid 6-digit pin code"
	}
	if err := apperr.AssertNoFieldErrors(fields); err != nil {
		return db.AddressFields{}, err
	}
	var line2 *string
	if in.HasLine2 {
		if v := strings.TrimSpace(in.Line2); v != "" {
			line2 = &v
		}
	}
	return db.AddressFields{
		Label: strings.TrimSpace(in.Label), Name: strings.TrimSpace(in.Name), Phone: in.Phone,
		Line1: strings.TrimSpace(in.Line1), Line2: line2, City: strings.TrimSpace(in.City), State: strings.TrimSpace(in.State), Pincode: in.Pincode,
	}, nil
}

// Service is the address-book business logic.
type Service struct{ store *db.Store }

// NewService builds the service.
func NewService(store *db.Store) *Service { return &Service{store: store} }

// List returns the user's addresses, oldest first.
func (s *Service) List(ctx context.Context, userID string) ([]domain.SavedAddress, error) {
	rows, err := db.AddressesFor(ctx, s.store.Pool, userID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SavedAddress, len(rows))
	for i := range rows {
		out[i] = ToSavedAddress(&rows[i])
	}
	return out, nil
}

// Add saves a new address; the first one becomes the default.
func (s *Service) Add(ctx context.Context, userID string, in Input) ([]domain.SavedAddress, error) {
	f, err := validate(in)
	if err != nil {
		return nil, err
	}
	count, err := db.CountAddresses(ctx, s.store.Pool, userID)
	if err != nil {
		return nil, err
	}
	if count >= MaxAddresses {
		return nil, apperr.New(apperr.Validation, "You can save up to 10 addresses.")
	}
	if err := db.CreateAddress(ctx, s.store.Pool, userID, f, count == 0); err != nil {
		return nil, err
	}
	return s.List(ctx, userID)
}

// Update edits one of the user's addresses. userID is in every query: one customer can never touch another's address.
func (s *Service) Update(ctx context.Context, userID, id string, in Input) ([]domain.SavedAddress, error) {
	f, err := validate(in)
	if err != nil {
		return nil, err
	}
	ok, err := db.UpdateAddress(ctx, s.store.Pool, id, userID, f)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.New(apperr.NotFound, "Address not found")
	}
	return s.List(ctx, userID)
}

// Remove deletes an address; removing the default promotes the oldest remaining one.
func (s *Service) Remove(ctx context.Context, userID, id string) ([]domain.SavedAddress, error) {
	err := s.store.Tx(ctx, func(tx pgx.Tx) error {
		removed, err := db.AddressOf(ctx, tx, id, userID)
		if err != nil || removed == nil {
			return err
		}
		if err := db.DeleteAddress(ctx, tx, id); err != nil {
			return err
		}
		if removed.IsDefault {
			next, err := db.OldestAddress(ctx, tx, userID)
			if err != nil {
				return err
			}
			if next != nil {
				return db.SetDefaultAddress(ctx, tx, next.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.List(ctx, userID)
}

// SetDefault makes one address the default.
func (s *Service) SetDefault(ctx context.Context, userID, id string) ([]domain.SavedAddress, error) {
	err := s.store.Tx(ctx, func(tx pgx.Tx) error {
		found, err := db.AddressOf(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if found == nil {
			return apperr.New(apperr.NotFound, "Address not found")
		}
		if err := db.ClearDefaults(ctx, tx, userID); err != nil {
			return err
		}
		return db.SetDefaultAddress(ctx, tx, id)
	})
	if err != nil {
		return nil, err
	}
	return s.List(ctx, userID)
}

// Handler serves /account/addresses.
type Handler struct {
	svc    *Service
	guards *httpx.Guards
}

// NewHandler builds the handler.
func NewHandler(svc *Service, guards *httpx.Guards) *Handler {
	return &Handler{svc: svc, guards: guards}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	g := h.guards.Require("address:write:own")
	mux.HandleFunc("GET /account/addresses", httpx.Wrap(httpx.Chain(h.list, g)))
	mux.HandleFunc("POST /account/addresses", httpx.Wrap(httpx.Chain(h.add, g)))
	mux.HandleFunc("PUT /account/addresses/{id}", httpx.Wrap(httpx.Chain(h.update, g)))
	mux.HandleFunc("DELETE /account/addresses/{id}", httpx.Wrap(httpx.Chain(h.remove, g)))
	mux.HandleFunc("POST /account/addresses/{id}/default", httpx.Wrap(httpx.Chain(h.setDefault, g)))
}

func readInput(r *http.Request) (Input, error) {
	body := httpx.ReadObject(r, "label", "name", "phone", "address")
	in := Input{Label: body.Str("label", 60), Name: body.Str("name", 120), Phone: body.Str("phone", 20)}
	addr := body.Object("address", "line1", "line2", "city", "state", "pincode")
	in.Line1 = addr.Str("line1", 200)
	in.Line2, in.HasLine2 = addr.OptStr("line2", 200)
	in.City = addr.Str("city", 100)
	in.State = addr.Str("state", 100)
	in.Pincode = addr.Str("pincode", 10)
	body.Absorb(addr)
	return in, body.Err()
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	out, err := h.svc.List(r.Context(), user.ID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	in, err := readInput(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Add(r.Context(), user.ID, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
	return nil
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	in, err := readInput(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Update(r.Context(), user.ID, r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	out, err := h.svc.Remove(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) setDefault(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	out, err := h.svc.SetDefault(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
