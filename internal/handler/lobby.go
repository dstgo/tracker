package handler

import (
	"context"
	"errors"
	"github.com/dstgo/tracker/internal/data/repo"
	"github.com/dstgo/tracker/internal/types"
	"github.com/dstgo/tracker/pkg/lobbyapi"
	"github.com/go-redis/redis/v8"
	"github.com/oschwald/geoip2-golang"
	"go.mongodb.org/mongo-driver/bson"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

type LobbyHandler interface {
	// GetServersByPage returns server list from database by given queryOptions
	GetServersByPage(ctx context.Context, queryOptions types.QueryLobbyServersOptions) (types.PageResult[types.QueryLobbyServersResp], error)
	// GetAllServersFromLobby collects and returns server information from klei lobby server
	GetAllServersFromLobby(ctx context.Context, limit int) ([]repo.LobbyServer, error)
	// SyncLocalServers collects server information from klei, process and store them into database,
	// then return how many records has been stored
	SyncLocalServers(ctx context.Context, limit int) (int, error)
	// ClearExpiredServers remove expired servers from database
	ClearExpiredServers(ctx context.Context, ttl time.Duration) (int64, int64, error)
	// GetServerDetails returns details information for specific server
	GetServerDetails(ctx context.Context, region, rowId string) (types.QueryLobbyServerDetailResp, error)
}

func NewLobbyMongoHandler(lobbyRepo *repo.LobbyRepo, redis *redis.Client, lobby *lobbyapi.Client, geoip *geoip2.Reader) *LobbyMongoHandler {
	return &LobbyMongoHandler{
		lobbyRepo: lobbyRepo,
		redis:     redis,
		lobby:     lobby,
		geoip:     geoip,
	}
}

var _ LobbyHandler = (*LobbyMongoHandler)(nil)

type LobbyMongoHandler struct {
	lobbyRepo *repo.LobbyRepo
	redis     *redis.Client
	lobby     *lobbyapi.Client
	geoip     *geoip2.Reader
}

func (l *LobbyMongoHandler) GetServersByPage(ctx context.Context, options types.QueryLobbyServersOptions) (types.PageResult[types.QueryLobbyServersResp], error) {
	queryM := map[string]any{}

	if options.Name != "" {
		queryM["name"] = bson.M{
			"$regex":   options.Name,
			"$options": "i",
		}
	}

	if options.Address != "" {
		queryM["address"] = options.Address
	}

	if options.Area != "" {
		queryM["area"] = options.Area
	}

	if options.Intent != "" {
		queryM["intent"] = options.Intent
	}

	if options.GameMode != "" {
		queryM["game_mode"] = options.GameMode
	}

	if options.PvpEnabled != 0 {
		queryM["pvp_enabled"] = options.PvpEnabled > 0
	}

	if options.HasPassword != 0 {
		queryM["has_password"] = options.PvpEnabled > 0
	}

	if options.ModEnabled != 0 {
		queryM["mod_enabled"] = options.ModEnabled > 0
	}

	// server tags
	if tags := strings.Split(options.Tags, ","); options.Tags != "" && len(tags) > 0 {
		queryM["tag_names"] = bson.M{
			"$in": tags,
		}
	}

	var pageResult types.PageResult[types.QueryLobbyServersResp]

	result, err := l.lobbyRepo.FindServers(ctx, options.Page, options.Size, options.Sort, queryM)
	if err != nil {
		return pageResult, err
	}
	pageResult.Total = result.Total
	pageResult.List = lobbyRepo2Resp(result.List)

	return pageResult, nil
}

func (l *LobbyMongoHandler) GetServerDetails(ctx context.Context, region, rowId string) (types.QueryLobbyServerDetailResp, error) {
	var result types.QueryLobbyServerDetailResp

	// get details
	details, err := l.lobby.GetServerDetails(region, rowId)
	if err != nil {
		return result, err
	}

	// process
	processList, err := processLobbyServer([]lobbyapi.Server{details.Server}, l.geoip, region, 0)
	if err != nil {
		return result, err
	}

	// it is impossible to occur at most time
	if len(processList) < 1 {
		return result, errors.New("lobby details: invalid processList")
	}

	result.QueryLobbyServersResp = lobbyRepo2Resp(processList)[0]
	result.Details = details.Details

	return result, nil
}

// GetAllServersFromLobby returns all lobby servers in parallel. Using limit params to limit the number of goroutine
func (l *LobbyMongoHandler) GetAllServersFromLobby(ctx context.Context, limit int) ([]repo.LobbyServer, error) {
	slog.Info("begin")

	regions, err := l.lobby.GetCapableRegions()
	if err != nil {
		return nil, err
	}

	ts := time.Now().UnixMilli()

	var servers []repo.LobbyServer
	// protect servers []repo.LobbyServer
	var mu sync.Mutex

	group, _ := errgroup.WithContext(ctx)
	group.SetLimit(limit)

	// request servers list from lobby server for each region and platforms
	// and process list parallelly
	for _, region := range regions.Regions {
		for _, platform := range lobbyapi.ExplicitPlatforms {
			group.Go(func() error {
				// get servers
				lobbyServers, err := l.lobby.GetLobbyServers(region.Region, platform)
				if err != nil {
					return err
				}

				// return if list is empty
				if len(lobbyServers.List) == 0 {
					return nil
				}

				// process
				processList, err := processLobbyServer(lobbyServers.List, l.geoip, region.Region, ts)
				if err != nil {
					return err
				}

				mu.Lock()
				servers = append(servers, processList...)
				mu.Unlock()

				return nil
			})
		}
	}

	// error occurred
	if err := group.Wait(); err != nil {
		return nil, err
	}

	return servers, nil
}

func (l *LobbyMongoHandler) ClearExpiredServers(ctx context.Context, ttl time.Duration) (int64, int64, error) {
	expiredTs := time.Now().Add(-ttl).UnixMilli()
	filter := bson.M{
		"created_at": bson.M{
			"$lte": expiredTs,
		},
	}

	deleted, total, err := l.lobbyRepo.RemoveServers(ctx, filter)
	if err != nil {
		return 0, 0, err
	}
	return deleted, total, nil
}

func (l *LobbyMongoHandler) SyncLocalServers(ctx context.Context, limit int) (int, error) {
	servers, err := l.GetAllServersFromLobby(ctx, limit)
	if err != nil {
		return 0, err
	}

	// store the server information into mongodb
	inserted, err := l.lobbyRepo.InsertManyServers(ctx, servers)
	if err != nil {
		return inserted, err
	}
	return inserted, nil
}

func lobbyRepo2Resp(servers []repo.LobbyServer) []types.QueryLobbyServersResp {
	var res []types.QueryLobbyServersResp
	for _, server := range servers {
		res = append(res, types.QueryLobbyServersResp{
			RowId:        server.RowId,
			SteamClanId:  server.SteamClanId,
			Address:      server.Address,
			Port:         server.Port,
			Host:         server.Host,
			Region:       server.Region,
			Continent:    server.Continent,
			Area:         server.Area,
			City:         server.City,
			PlatformName: server.PlatformName,
			Platform:     int(server.Platform),
			Version:      server.Version,
			Name:         server.Name,
			GameMode:     server.GameMode,
			Intent:       server.Intent,
			Season:       server.Season,
			// convert tags into array
			Tags:            server.TagNames,
			MaxPlayers:      server.MaxConnections,
			Online:          server.Connected,
			Mod:             server.ModEnabled,
			Pvp:             server.PvpEnabled,
			HasPassword:     server.HasPassword,
			IsDedicated:     server.IsDedicated,
			ClientHosted:    server.ClientHosted,
			AllowNewPlayers: server.AllowNewPlayers,
			ServerPaused:    server.ServerPaused,
			FriendOnly:      server.FriendOnly,
			ClanOnly:        server.ClanOnly,
		})
	}
	return res
}

func processLobbyServer(servers []lobbyapi.Server, geoip *geoip2.Reader, region string, ts int64) ([]repo.LobbyServer, error) {
	var ans []repo.LobbyServer
	for _, server := range servers {

		s := repo.LobbyServer{Region: region, Server: server, CreatedAt: ts}

		// tags
		if s.Tags != "" {
			s.TagNames = strings.Split(s.Tags, ",")
		}

		// geo information
		city, err := geoip.City(net.ParseIP(s.Address))
		if err != nil {
			return nil, err
		}

		s.Continent = city.Continent.Code
		s.Area = city.Country.IsoCode
		s.City = city.City.Names["en"]

		// display platform
		s.PlatformName = lobbyapi.PlatformDisplayName(s.Region, s.Platform)

		ans = append(ans, s)
	}

	return ans, nil
}
