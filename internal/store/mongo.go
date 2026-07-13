package store

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/esosaoh/crawl/internal/engine"
)

type Mongo struct {
	client *mongo.Client
	scans  *mongo.Collection
	links  *mongo.Collection
}

type linkDoc struct {
	URL          string       `bson:"_id"`
	Class        engine.Class `bson:"class"`
	Status       int          `bson:"status"`
	ETag         string       `bson:"etag,omitempty"`
	LastModified string       `bson:"last_modified,omitempty"`
	CheckedAt    time.Time    `bson:"checked_at"`
	Fails        int          `bson:"fails"`
	Successes    int          `bson:"successes"`
}

func NewMongo(ctx context.Context, uri, dbName string) (*Mongo, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		client.Disconnect(context.Background())
		return nil, err
	}
	db := client.Database(dbName)
	m := &Mongo{
		client: client,
		scans:  db.Collection("scans"),
		links:  db.Collection("links"),
	}
	m.scans.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "created_at", Value: -1}},
	})
	m.links.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "checked_at", Value: -1}},
	})
	return m, nil
}

func (m *Mongo) GetStates(ctx context.Context, urls []string) (map[string]*engine.LinkState, error) {
	cur, err := m.links.Find(ctx, bson.M{"_id": bson.M{"$in": urls}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := make(map[string]*engine.LinkState)
	for cur.Next(ctx) {
		var d linkDoc
		if err := cur.Decode(&d); err != nil {
			return nil, err
		}
		out[d.URL] = &engine.LinkState{
			URL:          d.URL,
			Class:        d.Class,
			Status:       d.Status,
			ETag:         d.ETag,
			LastModified: d.LastModified,
			CheckedAt:    d.CheckedAt,
			Fails:        d.Fails,
			Successes:    d.Successes,
		}
	}
	return out, cur.Err()
}

func (m *Mongo) PutStates(ctx context.Context, states []*engine.LinkState) error {
	if len(states) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(states))
	for _, s := range states {
		doc := linkDoc{
			URL:          s.URL,
			Class:        s.Class,
			Status:       s.Status,
			ETag:         s.ETag,
			LastModified: s.LastModified,
			CheckedAt:    s.CheckedAt,
			Fails:        s.Fails,
			Successes:    s.Successes,
		}
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"_id": s.URL}).
			SetReplacement(doc).
			SetUpsert(true))
	}
	_, err := m.links.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

func (m *Mongo) CreateScan(ctx context.Context, rec *ScanRecord) error {
	_, err := m.scans.InsertOne(ctx, rec)
	return err
}

func (m *Mongo) UpdateScan(ctx context.Context, rec *ScanRecord) error {
	_, err := m.scans.ReplaceOne(ctx, bson.M{"_id": rec.ID}, rec)
	return err
}

func (m *Mongo) GetScan(ctx context.Context, id string) (*ScanRecord, error) {
	var rec ScanRecord
	err := m.scans.FindOne(ctx, bson.M{"_id": id}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (m *Mongo) ListScans(ctx context.Context, limit int) ([]*ScanRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(int64(limit)).
		SetProjection(bson.M{"report.results": 0}) // list view doesn't need full results
	cur, err := m.scans.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*ScanRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Mongo) Close(ctx context.Context) error {
	return m.client.Disconnect(ctx)
}
