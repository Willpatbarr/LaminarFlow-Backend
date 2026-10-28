/*
╔═ bootstrap.go ════════════════════════════════════════════════════════════════════════
║  http handlers · team and project creation, and their lists
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeTeamNotFound       const
║      CodeTeamNameTaken      const
║      CodeProjectNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  POST /api/v1/teams · POST /api/v1/projects
║                     POST /api/v1/teams/list · POST /api/v1/projects/list
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
	AspectTypes []string     `json:"aspect_types" doc:"Seeded on creation. Starter types, not system types. Empty on get and list, which do not load them."`
}

type teamOutput struct {
	Body teamBody
}

type createTeamInput struct {
	Body struct {
		WorkspaceID string `json:"workspace_id" required:"true" format:"uuid"`
		Name        string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		Bare        bool   `json:"bare,omitempty" default:"false" doc:"Skip seeding. For an importer restoring a team that already has its own statuses and aspect types. Optional; omitted means false, a fully seeded team."`
	}
}

type teamInput struct {
	ID string `path:"id" format:"uuid"`
}

type projectColumnBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	StatusID string `json:"status_id"`
}

type projectBody struct {
	ID      string              `json:"id"`
	TeamID  string              `json:"team_id"`
	Name    string              `json:"name"`
	BoardID *string             `json:"board_id" doc:"The board seeded on creation. Null when bare - a project with no board is legal."`
	Columns []projectColumnBody `json:"columns" doc:"One per status the team had. Empty when bare."`
}

type projectOutput struct {
	Body projectBody
}

type createProjectInput struct {
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
		Name   string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		Bare   bool   `json:"bare,omitempty" default:"false" doc:"Skip the board. A project with no board is legal and renders empty. Optional; omitted means false, a project with its board."`
	}
}

type projectInput struct {
	ID string `path:"id" format:"uuid"`
}

func asStatusBody(st team.Status) statusBody {
	return statusBody{ID: st.ID, Name: st.Name, Color: st.Color, Position: st.Position, Category: st.Category}
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
		out.Statuses[i] = asStatusBody(st)
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
		Columns: make([]projectColumnBody, len(p.Columns)),
	}
	if p.BoardID != "" {
		out.BoardID = &p.BoardID
	}
	for i, c := range p.Columns {
		out.Columns[i] = projectColumnBody{ID: c.ID, Name: c.Name, Position: c.Position, StatusID: c.StatusID}
	}

	return out
}

// teamError separates the two failures a creation can have. A name collision is a 409:
// the request is fine and the current state refuses it, and sending it again under
// another name succeeds - which is exactly the line docs/api-errors.md draws.
func teamError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

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
	if e := limitError(err); e != nil {
		return e
	}

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

	huma.Register(api, huma.Operation{
		OperationID: "create-team",
		Method:      http.MethodPost,
		Path:        V1 + "/teams",
		Summary:     "Create a team, with the rows it needs to be usable",
		Description: "Seeds three statuses, five starter aspect types and one shared saved view, in the same " +
			"transaction that creates the team - so a team is never observable half-configured. " +
			"Everything seeded is an ordinary row: rename it, delete it, replace it. Nothing is privileged, " +
			"and no default labels are seeded, because a label vocabulary is a team's own.",
		Metadata: needs(auth.TeamWrite),
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
		Metadata:    needs(auth.TeamRead),
	}, func(ctx context.Context, in *teamInput) (*teamOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := teamSvc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, teamError(err)
		}

		return &teamOutput{Body: asTeamBody(t)}, nil
	})

	// The team list is where a client learns statuses: they ride on each team, with
	// category and position, so one call fills the store and a second is never needed.
	// Three statuses a team keeps the payload small; if that stops being true, the
	// answer is POST /statuses/list beside this, not a heavier team.
	registerList(api, listSpec[teamBody]{
		OperationID: "list-teams",
		Permission:  auth.TeamRead,
		Path:        V1 + "/teams/list",
		Summary:     "List the workspace's teams, each with its statuses (a read - see the description for why it is a POST)",
		Fields:      team.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[teamBody], error) {
			p := team.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := teamSvc.List(ctx, accountID, p)
			if err != nil {
				return listPage[teamBody]{}, err
			}

			items := make([]teamBody, len(page.Teams))
			for i, t := range page.Teams {
				items[i] = asTeamBody(t)
			}

			return listPage[teamBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-project",
		Method:      http.MethodPost,
		Path:        V1 + "/projects",
		Summary:     "Create a project, with the board it renders with",
		Description: "Seeds one board with a column per status the team has, each mapped to that status. " +
			"This is the second bootstrap point: board is project-scoped, so it cannot be seeded when the " +
			"team is created. A project created from a bare team gets a board with no columns.",
		Metadata: needs(auth.ProjectWrite),
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
		Metadata:    needs(auth.ProjectRead),
	}, func(ctx context.Context, in *projectInput) (*projectOutput, error) {
		me, _ := auth.FromContext(ctx)

		p, err := projSvc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, projectError(err)
		}

		return &projectOutput{Body: asProjectBody(p)}, nil
	})

	registerList(api, listSpec[projectBody]{
		OperationID: "list-projects",
		Permission:  auth.ProjectRead,
		Path:        V1 + "/projects/list",
		Summary:     "List the workspace's projects (a read - see the description for why it is a POST)",
		Fields:      project.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[projectBody], error) {
			p := project.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := projSvc.List(ctx, accountID, p)
			if err != nil {
				return listPage[projectBody]{}, err
			}

			items := make([]projectBody, len(page.Projects))
			for i, pr := range page.Projects {
				items[i] = asProjectBody(pr)
			}

			return listPage[projectBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})
}
