// Package catalog is the shop's catalog: products, categories, brands and reviews in MongoDB, search in
// Meilisearch, and the back-office product management that keeps the two in step.
package catalog

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/shubhamrathi020/eCommerce-go/internal/config"
)

// Mongo is the catalog store. Documents mirror the shared model shapes exactly; _id is the same string id used
// everywhere else, so mapping to and from the contract is the identity function. The driver uses the models'
// `json` tags as field names, and nested documents decode to maps so they can be returned as JSON untouched.
type Mongo struct {
	client *mongo.Client
	db     *mongo.Database
}

// OpenMongo connects to the catalog store.
func OpenMongo(ctx context.Context, cfg *config.Config) (*Mongo, error) {
	opts := options.Client().ApplyURI(cfg.MongoURL).SetBSONOptions(&options.BSONOptions{UseJSONStructTags: true, DefaultDocumentM: true})
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	m := &Mongo{client: client, db: client.Database(cfg.MongoDBName)}
	if err := m.Ping(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	return m, nil
}

// Ping checks the store answers.
func (m *Mongo) Ping(ctx context.Context) error {
	return m.db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err()
}

// Close disconnects.
func (m *Mongo) Close() { _ = m.client.Disconnect(context.Background()) }

// Products is the products collection.
func (m *Mongo) Products() *mongo.Collection { return m.db.Collection("products") }

// Categories is the categories collection.
func (m *Mongo) Categories() *mongo.Collection { return m.db.Collection("categories") }

// Brands is the brands collection.
func (m *Mongo) Brands() *mongo.Collection { return m.db.Collection("brands") }

// Collections is the collections collection.
func (m *Mongo) Collections() *mongo.Collection { return m.db.Collection("collections") }

// Reviews is the reviews collection.
func (m *Mongo) Reviews() *mongo.Collection { return m.db.Collection("reviews") }

// Home is the home-page content collection.
func (m *Mongo) Home() *mongo.Collection { return m.db.Collection("home") }

// optionsSortLimit sorts ascending by one field and caps the result.
func optionsSortLimit(field string, n int64) *options.FindOptionsBuilder {
	return options.Find().SetSort(bson.D{{Key: field, Value: 1}}).SetLimit(n)
}

// optionsLimit caps a Find result.
func optionsLimit(n int64) *options.FindOptionsBuilder { return options.Find().SetLimit(n) }
