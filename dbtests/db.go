package main

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var client *mongo.Client

type Memory struct {
	Version     int      `json:"version"`
	Summary     string   `json:"summary"`
	Context     string   `json:"context"`
	Description string   `json:"description"`
	Keywords    []string `json:"keywords"`
}

func main() {
	uri := "mongodb://root:example@localhost:27017/mydb?authSource=admin" //os.Getenv("MONGODB_URI")
	defer func() {
		if err := client.Disconnect(context.TODO()); err != nil {
			panic(err)
		}
	}()
	collection := initDBConn(uri, "harness", "memories")
	CreateMemory(context.TODO(), collection, "test summary2", "full context", "desc", []string{"la", "laa", "ha"})
}

func initDBConn(uri, dbName, colName string) *mongo.Collection {
	var err error
	client, err = mongo.Connect(options.Client().
		ApplyURI(uri).
		SetAuth(options.Credential{Username: "root", Password: "example"}))
	if err != nil {
		panic(err)
	}

	err = client.Database(dbName).CreateCollection(context.TODO(), colName)
	if err != nil {
		panic(err)
	}
	return client.Database(dbName).Collection(colName)
}

func FindMemory(ctx context.Context, coll *mongo.Collection, keywords []string) ([]Memory, error) {
	filter := bson.M{"keywords": bson.M{"$in": keywords}}

	curson, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer curson.Close(ctx)

	var memory []Memory
	if err := curson.All(ctx, &memory); err != nil {
		return nil, err
	}
	return memory, nil
}

func CreateMemory(ctx context.Context, coll *mongo.Collection, summary, fullContext, desc string, keywords []string) (*mongo.InsertOneResult, error) {
	return coll.InsertOne(ctx, Memory{
		Version:     0,
		Summary:     summary,
		Context:     fullContext,
		Keywords:    keywords,
		Description: desc,
	})
}
