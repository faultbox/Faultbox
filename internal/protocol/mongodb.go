package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func init() {
	Register(&mongoProtocol{})
}

type mongoProtocol struct{}

func (p *mongoProtocol) Name() string { return "mongodb" }

func (p *mongoProtocol) Methods() []string {
	return []string{"find", "insert", "insert_many", "update", "delete", "count", "command"}
}

func (p *mongoProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	if err := TCPHealthcheck(ctx, addr, timeout); err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client, err := p.connect(pingCtx, addr, "")
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer client.Disconnect(pingCtx)
	return client.Ping(pingCtx, nil)
}

func (p *mongoProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	start := time.Now()
	db := getStringKwarg(kwargs, "database", "test")

	client, err := p.connect(ctx, addr, db)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("connect: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer client.Disconnect(ctx)

	database := client.Database(db)

	switch method {
	case "find":
		return p.executeFind(ctx, database, kwargs, start)
	case "insert":
		return p.executeInsert(ctx, database, kwargs, start)
	case "insert_many":
		return p.executeInsertMany(ctx, database, kwargs, start)
	case "update":
		return p.executeUpdate(ctx, database, kwargs, start)
	case "delete":
		return p.executeDelete(ctx, database, kwargs, start)
	case "count":
		return p.executeCount(ctx, database, kwargs, start)
	case "command":
		return p.executeCommand(ctx, database, kwargs, start)
	default:
		return nil, fmt.Errorf("unsupported mongodb method %q", method)
	}
}

func (p *mongoProtocol) connect(ctx context.Context, addr, db string) (*mongo.Client, error) {
	uri := fmt.Sprintf("mongodb://%s", addr)
	opts := options.Client().ApplyURI(uri).SetConnectTimeout(5 * time.Second).SetServerSelectionTimeout(5 * time.Second)
	return mongo.Connect(opts)
}

func (p *mongoProtocol) executeFind(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.find requires collection= argument")
	}

	filter, err := parseBSONFromKwarg(kwargs, "filter")
	if err != nil {
		return failStep(start, fmt.Sprintf("filter: %v", err)), nil
	}

	opts := options.Find()
	if limit := getIntKwarg(kwargs, "limit", 0); limit > 0 {
		opts.SetLimit(limit)
	}

	cursor, err := db.Collection(collection).Find(ctx, filter, opts)
	if err != nil {
		return failStep(start, err.Error()), nil
	}
	defer cursor.Close(ctx)

	var results []bson.M
	if err := cursor.All(ctx, &results); err != nil {
		return failStep(start, fmt.Sprintf("cursor: %v", err)), nil
	}

	body, _ := json.Marshal(normalizeBSONSlice(results))
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"documents": fmt.Sprintf("%d", len(results)), "collection": collection},
	}, nil
}

func (p *mongoProtocol) executeInsert(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.insert requires collection= argument")
	}

	doc, err := parseBSONFromKwarg(kwargs, "document")
	if err != nil {
		return failStep(start, fmt.Sprintf("document: %v", err)), nil
	}

	res, err := db.Collection(collection).InsertOne(ctx, doc)
	if err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(map[string]any{
		"inserted_id": fmt.Sprintf("%v", res.InsertedID),
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"collection": collection, "op": "insert"},
	}, nil
}

func (p *mongoProtocol) executeInsertMany(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.insert_many requires collection= argument")
	}

	raw, ok := kwargs["documents"]
	if !ok {
		return nil, fmt.Errorf("mongodb.insert_many requires documents= argument")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("mongodb.insert_many documents= must be a list")
	}

	docs := make([]any, 0, len(list))
	for i, item := range list {
		doc, err := toBSONDoc(item)
		if err != nil {
			return failStep(start, fmt.Sprintf("documents[%d]: %v", i, err)), nil
		}
		docs = append(docs, doc)
	}

	res, err := db.Collection(collection).InsertMany(ctx, docs)
	if err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(map[string]any{
		"inserted_count": len(res.InsertedIDs),
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"collection": collection, "op": "insert"},
	}, nil
}

func (p *mongoProtocol) executeUpdate(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.update requires collection= argument")
	}

	filter, err := parseBSONFromKwarg(kwargs, "filter")
	if err != nil {
		return failStep(start, fmt.Sprintf("filter: %v", err)), nil
	}
	update, err := parseBSONFromKwarg(kwargs, "update")
	if err != nil {
		return failStep(start, fmt.Sprintf("update: %v", err)), nil
	}

	res, err := db.Collection(collection).UpdateMany(ctx, filter, update)
	if err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(map[string]any{
		"matched":  res.MatchedCount,
		"modified": res.ModifiedCount,
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"collection": collection, "op": "update"},
	}, nil
}

func (p *mongoProtocol) executeDelete(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.delete requires collection= argument")
	}

	filter, err := parseBSONFromKwarg(kwargs, "filter")
	if err != nil {
		return failStep(start, fmt.Sprintf("filter: %v", err)), nil
	}

	res, err := db.Collection(collection).DeleteMany(ctx, filter)
	if err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(map[string]any{
		"deleted": res.DeletedCount,
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"collection": collection, "op": "delete"},
	}, nil
}

func (p *mongoProtocol) executeCount(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	collection := getStringKwarg(kwargs, "collection", "")
	if collection == "" {
		return nil, fmt.Errorf("mongodb.count requires collection= argument")
	}

	filter, err := parseBSONFromKwarg(kwargs, "filter")
	if err != nil {
		return failStep(start, fmt.Sprintf("filter: %v", err)), nil
	}
	if filter == nil {
		filter = bson.M{}
	}

	count, err := db.Collection(collection).CountDocuments(ctx, filter)
	if err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(map[string]any{"count": count})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"collection": collection, "op": "count"},
	}, nil
}

func (p *mongoProtocol) executeCommand(ctx context.Context, db *mongo.Database, kwargs map[string]any, start time.Time) (*StepResult, error) {
	cmd, err := parseBSONFromKwarg(kwargs, "cmd")
	if err != nil {
		return failStep(start, fmt.Sprintf("cmd: %v", err)), nil
	}
	if cmd == nil {
		return nil, fmt.Errorf("mongodb.command requires cmd= argument")
	}

	var res bson.M
	if err := db.RunCommand(ctx, cmd).Decode(&res); err != nil {
		return failStep(start, err.Error()), nil
	}

	body, _ := json.Marshal(normalizeBSONDoc(res))
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func failStep(start time.Time, msg string) *StepResult {
	return &StepResult{
		Success:    false,
		Error:      msg,
		DurationMs: time.Since(start).Milliseconds(),
	}
}

// parseBSONFromKwarg accepts either a map[string]any (from Starlark dict) or a
// JSON string and converts it to bson.M. Returns nil if the kwarg is absent.
func parseBSONFromKwarg(kwargs map[string]any, key string) (bson.M, error) {
	raw, ok := kwargs[key]
	if !ok || raw == nil {
		return nil, nil
	}
	return toBSONDoc(raw)
}

func toBSONDoc(v any) (bson.M, error) {
	switch val := v.(type) {
	case map[string]any:
		return normalizeForBSON(val), nil
	case string:
		if val == "" {
			return nil, nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(val), &m); err != nil {
			return nil, fmt.Errorf("invalid JSON: %v", err)
		}
		return normalizeForBSON(m), nil
	default:
		return nil, fmt.Errorf("expected dict or JSON string, got %T", v)
	}
}

// normalizeForBSON recursively converts map[string]any / []any trees into
// bson.M / bson.A so the driver encodes them correctly.
func normalizeForBSON(m map[string]any) bson.M {
	out := make(bson.M, len(m))
	for k, v := range m {
		out[k] = normalizeValueForBSON(v)
	}
	return out
}

func normalizeValueForBSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return normalizeForBSON(val)
	case []any:
		arr := make(bson.A, len(val))
		for i, item := range val {
			arr[i] = normalizeValueForBSON(item)
		}
		return arr
	default:
		return v
	}
}

// normalizeBSONDoc converts bson.M back to JSON-safe map[string]any
// (stringifies ObjectIDs and other non-JSON types).
func normalizeBSONDoc(doc bson.M) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = normalizeBSONValue(v)
	}
	return out
}

func normalizeBSONSlice(docs []bson.M) []map[string]any {
	out := make([]map[string]any, len(docs))
	for i, d := range docs {
		out[i] = normalizeBSONDoc(d)
	}
	return out
}

func normalizeBSONValue(v any) any {
	switch val := v.(type) {
	case bson.M:
		return normalizeBSONDoc(val)
	case bson.A:
		arr := make([]any, len(val))
		for i, item := range val {
			arr[i] = normalizeBSONValue(item)
		}
		return arr
	case bson.ObjectID:
		return val.Hex()
	case bson.DateTime:
		return val.Time().UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// getIntKwarg reads an int64 kwarg with a default.
func getIntKwarg(kwargs map[string]any, key string, def int64) int64 {
	raw, ok := kwargs[key]
	if !ok {
		return def
	}
	switch v := raw.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return def
	}
}
