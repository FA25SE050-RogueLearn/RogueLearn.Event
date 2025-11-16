package user

import (
	"context"
	"crypto/tls"
	"strings"

	pb "github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Client struct {
	conn               *grpc.ClientConn
	userProfilesClient pb.UserProfilesServiceClient
	userContextClient  pb.UserContextServiceClient
	achievementsClient pb.AchievementsServiceClient
	guildsClient       pb.GuildsServiceClient
}

func NewClient(addr string) (*Client, error) {
	// Check if the original address had https:// to determine if TLS is needed
	useTLS := strings.HasPrefix(addr, "https://")

	// Strip http:// or https:// prefix if present, as gRPC doesn't use them
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")

	var opts []grpc.DialOption
	if useTLS {
		// Use TLS with support for self-signed certificates (for development)
		// In production, you should validate certificates properly
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true, // Skip certificate verification for self-signed certs
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:               conn,
		userProfilesClient: pb.NewUserProfilesServiceClient(conn),
		userContextClient:  pb.NewUserContextServiceClient(conn),
		achievementsClient: pb.NewAchievementsServiceClient(conn),
		guildsClient:       pb.NewGuildsServiceClient(conn),
	}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// UserProfilesService methods
func (c *Client) GetUserProfileByAuthId(ctx context.Context, req *pb.GetUserProfileByAuthIdRequest) (*pb.UserProfile, error) {
	return c.userProfilesClient.GetByAuthId(ctx, req)
}

func (c *Client) UpdateMyProfile(ctx context.Context, req *pb.UpdateMyProfileRequest) (*pb.UserProfile, error) {
	return c.userProfilesClient.UpdateMyProfile(ctx, req)
}

func (c *Client) LogNewUser(ctx context.Context, req *pb.LogNewUserRequest) (*emptypb.Empty, error) {
	return c.userProfilesClient.LogNewUser(ctx, req)
}

func (c *Client) GetAllUserProfiles(ctx context.Context, req *pb.GetAllUserProfilesRequest) (*pb.UserProfileList, error) {
	return c.userProfilesClient.GetAll(ctx, req)
}

// UserContextService methods
func (c *Client) GetUserContextByAuthId(ctx context.Context, req *pb.GetUserContextByAuthIdRequest) (*pb.UserContext, error) {
	return c.userContextClient.GetByAuthId(ctx, req)
}

// AchievementsService methods
func (c *Client) GetAllAchievements(ctx context.Context, req *pb.GetAllRequest) (*pb.AchievementList, error) {
	return c.achievementsClient.GetAll(ctx, req)
}

func (c *Client) CreateAchievement(ctx context.Context, req *pb.CreateAchievementRequest) (*pb.Achievement, error) {
	return c.achievementsClient.Create(ctx, req)
}

func (c *Client) UpdateAchievement(ctx context.Context, req *pb.UpdateAchievementRequest) (*pb.Achievement, error) {
	return c.achievementsClient.Update(ctx, req)
}

func (c *Client) DeleteAchievement(ctx context.Context, req *pb.DeleteAchievementRequest) (*emptypb.Empty, error) {
	return c.achievementsClient.Delete(ctx, req)
}

func (c *Client) AwardAchievementToUser(ctx context.Context, req *pb.AwardAchievementToUserRequest) (*emptypb.Empty, error) {
	return c.achievementsClient.AwardToUser(ctx, req)
}

func (c *Client) RevokeAchievementFromUser(ctx context.Context, req *pb.RevokeAchievementFromUserRequest) (*emptypb.Empty, error) {
	return c.achievementsClient.RevokeFromUser(ctx, req)
}

func (c *Client) GetUserAchievementsByAuthUserId(ctx context.Context, req *pb.GetUserAchievementsByAuthUserIdRequest) (*pb.UserAchievementList, error) {
	return c.achievementsClient.GetByAuthUserId(ctx, req)
}

// GuildsService methods
func (c *Client) GetGuildById(ctx context.Context, req *pb.GetGuildByIdRequest) (*pb.Guild, error) {
	return c.guildsClient.GetGuildById(ctx, req)
}

func (c *Client) GetAllGuilds(ctx context.Context, req *pb.GetAllGuildsRequest) (*pb.GuildList, error) {
	return c.guildsClient.GetAllGuilds(ctx, req)
}

func (c *Client) GetGuildInvitations(ctx context.Context, req *pb.GetGuildInvitationsRequest) (*pb.GuildInvitationList, error) {
	return c.guildsClient.GetInvitations(ctx, req)
}

func (c *Client) GetGuildJoinRequests(ctx context.Context, req *pb.GetGuildJoinRequestsRequest) (*pb.GuildJoinRequestList, error) {
	return c.guildsClient.GetJoinRequests(ctx, req)
}

func (c *Client) GetGuildMemberRoles(ctx context.Context, req *pb.GetGuildMemberRolesRequest) (*pb.GuildMemberRoleList, error) {
	return c.guildsClient.GetMemberRoles(ctx, req)
}

func (c *Client) GetMyGuild(ctx context.Context, req *pb.GetMyGuildRequest) (*pb.Guild, error) {
	return c.guildsClient.GetMyGuild(ctx, req)
}

func (c *Client) GetMyJoinRequests(ctx context.Context, req *pb.GetMyJoinRequestsRequest) (*pb.GuildJoinRequestList, error) {
	return c.guildsClient.GetMyJoinRequests(ctx, req)
}
