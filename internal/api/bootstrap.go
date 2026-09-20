/*
╔═ bootstrap.go ════════════════════════════════════════════════════════════════════════
║  http handlers · team and project creation
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeTeamNotFound       const
║      CodeTeamNameTaken      const
║      CodeProjectNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  POST /api/v1/teams · POST /api/v1/projects
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"

	"github.com/danielgtaylor/huma/v2"
)

const (
	CodeTeamNotFound    = "team_not_found"
	CodeTeamNameTaken   = "team_name_taken"
	CodeProjectNotFound = "project_not_found"
)

// Creation responses carry what was seeded rather than only the new id, so a client can
// render the team or the board without a second round trip - and so that "what did
// bootstrap actually do" is answerable from the response rather than from the source.

type statusBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
	Category string `json:"category" doc:"not_started, in_progress or done. Reporting depends on it, and it survives a rename."`
}

type teamBody struct {
	ID          string       `json:"id"`
	WorkspaceID string       `json:"workspace_id"`
	Name        string       `json:"name"`
	Statuses    []statusBody `json:"statuses" doc:"Seeded on creation. Ordinary rows - rename or delete them freely."`
	AspectTypes []string     `json:"aspect_types" doc:"Seeded on creation. Starter types, not system types."`
}

type teamOutput struct {
	Body teamBody
}

type createTeamInput struct {
	Body struct {
		WorkspaceID string `json:"workspace_id" required:"true" format:"uuid"`
		Name        string `json:"name" required:"true" minLength:"1"`
		Bare        bool   `json:"bare" doc:"Skip seeding. For an importer restoring a team that already has its own statuses and aspect types."`
	}
}

type teamInput struct {
	ID string `path:"id" format:"uuid"`
}

type columnBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	StatusID string `json:"status_id"`
}

type projectBody struct {
	ID      string       `json:"id"`
	TeamID  string       `json:"team_id"`
	Name    string       `json:"name"`
	BoardID *string      `json:"board_id" doc:"The board seeded on creation. Null when bare - a project with no board is legal."`
	Columns []columnBody `json:"columns" doc:"One per status the team had. Empty when bare."`
}

type projectOutput struct {
	Body projectBody
}

type createProjectInput struct {
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
		Name   string `json:"name" required:"true" minLength:"1"`
		Bare   bool   `json:"bare" doc:"Skip the board. A project with no board is legal and renders empty."`
	}
}

type projectInput struct {
	ID string `path:"id" format:"uuid"`
}

func asTeamBody(t team.Team) teamBody {
	out := teamBody{
		ID:          t.ID,
		WorkspaceID: t.WorkspaceID,
		Name:        t.Name,
		Statuses:    make([]statusBody, len(t.Statuses)),
		AspectTypes: t.AspectTypes,
	}
	for i, st := range t.Statuses {
		out.Statuses[i] = statusBody{
			ID: st.ID, Name: st.Name, Color: st.Color,
			Position: st.Position, Category: st.Category,
		}
	}
	if out.AspectTypes == nil {
		out.AspectTypes = []string{}
	}

	return out
}

func asProjectBody(p project.Project) projectBody {
	out := projectBody{
		ID:      p.ID,
		TeamID:  p.TeamID,
		Name:    p.Name,
		Columns: make([]columnBody, len(p.Columns)),
	}
	if p.BoardID != "" {
		out.BoardID = &p.BoardID
	}
	for i, c := range p.Columns {
		out.Columns[i] = columnBody{ID: c.ID, Name: c.Name, Position: c.Position, StatusID: c.StatusID}
	}

	return out
}

// teamError separates the two failures a creation can have. A name collision is a 409:
// the request is fine and the current state refuses it, and sending it again under
// another name succeeds - which is exactly the line docs/api-errors.md draws.
func teamError(err error) error {
	switch {
	case errors.Is(err, team.ErrNotFound):
		return NewError(http.StatusNotFound, CodeTeamNotFound, "no such workspace")
	case errors.Is(err, team.ErrNameTaken):
		return NewError(http.StatusConflict, CodeTeamNameTaken,
			"a team of that name already exists in this workspace")
	}

	return err
}

func projectError(err error) error {
	if errors.Is(err, project.ErrNotFound) {
		return NewError(http.StatusNotFound, CodeProjectNotFound, "no such team or project")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers team and project creation
├─ in ────────────────────────────────────────────
│      api        huma.API
│      teamSvc    *team.Service       nil only for cmd/openapi
│      projSvc    *project.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/teams
*/

func registerBootstrap(api huma.API, teamSvc *team.Service, projSvc *project.Service) {
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-team",
		Method:      http.MethodPost,
		Path:        V1 + "/teams",
		Summary:     "Create a team, with the rows it needs to be usable",
		Description: "Seeds three statuses, five starter aspect types and one shared saved view, in the same " +
			"transaction that creates the team - so a team is never observable half-configured. " +
			"Everything seeded is an ordinary row: rename it, delete it, replace it. Nothing is privileged, " +
			"and no default labels are seeded, because a label vocabulary is a team's own.",
		Metadata: requires,
	}, func(ctx context.Context, in *createTeamInput) (*teamOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := teamSvc.Create(ctx, me.AccountID, team.CreateParams{
			WorkspaceID: in.Body.WorkspaceID,
			Name:        in.Body.Name,
			Bare:        in.Body.Bare,
		})
		if err != nil {
			return nil, teamError(err)
		}

		return &teamOutput{Body: asTeamBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-team",
		Method:      http.MethodGet,
		Path:        V1 + "/teams/{id}",
		Summary:     "Fetch one team with its statuses",
		Metadata:    requires,
	}, func(ctx context.Context, in *teamInput) (*teamOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := teamSvc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, teamError(err)
		}

		return &teamOutput{Body: asTeamBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-project",
		Method:      http.MethodPost,
		Path:        V1 + "/projects",
		Summary:     "Create a project, with the board it renders with",
		Description: "Seeds one board with a column per status the team has, each mapped to that status. " +
			"This is the second bootstrap point: board is project-scoped, so it cannot be seeded when the " +
			"team is created. A project created from a bare team gets a board with no columns.",
		Metadata: requires,
	}, func(ctx context.Context, in *createProjectInput) (*projectOutput, error) {
		me, _ := auth.FromContext(ctx)

		p, err := projSvc.Create(ctx, me.AccountID, project.CreateParams{
			TeamID: in.Body.TeamID,
			Name:   in.Body.Name,
			Bare:   in.Body.Bare,
		})
		if err != nil {
			return nil, projectError(err)
		}

		return &projectOutput{Body: asProjectBody(p)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-project",
		Method:      http.MethodGet,
		Path:        V1 + "/projects/{id}",
		Summary:     "Fetch one project",
		Metadata:    requires,
	}, func(ctx context.Context, in *projectInput) (*projectOutput, error) {
		me, _ := auth.FromContext(ctx)

		p, err := projSvc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, projectError(err)
		}

		return &projectOutput{Body: asProjectBody(p)}, nil
	})
}
