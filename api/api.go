package api

import (
	"context"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/dstgo/tracker/internal/data/repo"
	"github.com/dstgo/tracker/internal/handler"
	"github.com/dstgo/tracker/internal/types"
)

type API struct {
	Sys   SystemAPI
	Lobby LobbyAPI
}

// NewRouter registers http router handlers
func NewRouter(ctx context.Context, hertz *server.Hertz, env *types.Env) (*API, error) {

	hlog.Debug("initializing data repo and creating db index")
	statisticRepo := repo.NewLobbyStatisticRepo(env.MongoDB)
	// repositories
	lobbyRepo, err := repo.NewLobbyRepo(ctx, env.MongoDB)
	if err != nil {
		return nil, err
	}

	// handler
	lobbyMongoHandler := handler.NewLobbyMongoHandler(lobbyRepo, statisticRepo, env.LobbyCLI, env.GeoIpDB)
	modHandler := handler.NewWorkShopHandler(env.SteamCLI)

	// system api
	sysAPI := SystemAPI{}
	hertz.GET("/ts", sysAPI.Ts)

	// lobby api
	lobbyAPI := LobbyAPI{LobbyHandler: lobbyMongoHandler}
	hertz.GET("/lobby/list", lobbyAPI.List)
	hertz.GET("/lobby/details", lobbyAPI.Details)
	hertz.GET("/lobby/stat", lobbyAPI.Statistic)

	modAPI := ModAPI{modHandler: modHandler}
	hertz.GET("/mod/search", modAPI.Search)

	// mod api

	return &API{
		Sys:   sysAPI,
		Lobby: lobbyAPI,
	}, nil
}
