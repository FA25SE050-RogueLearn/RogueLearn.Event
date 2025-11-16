package executor

import (
	"context"
	"strings"

	pb "github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn   *grpc.ClientConn
	client pb.ExecutorServiceClient
}

func NewClient(addr string) (*Client, error) {
	// Strip http:// or https:// prefix if present, as gRPC doesn't use them
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:   conn,
		client: pb.NewExecutorServiceClient(conn),
	}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) ExecuteCode(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	return c.client.Execute(ctx, req)
}
