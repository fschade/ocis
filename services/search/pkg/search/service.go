package search

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	user "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	revactx "github.com/cs3org/reva/v2/pkg/ctx"
	"github.com/cs3org/reva/v2/pkg/errtypes"
	"github.com/cs3org/reva/v2/pkg/rgrpc/todo/pool"
	sdk "github.com/cs3org/reva/v2/pkg/sdk/common"
	"github.com/cs3org/reva/v2/pkg/storage/utils/walker"
	"github.com/cs3org/reva/v2/pkg/storagespace"
	"github.com/cs3org/reva/v2/pkg/utils"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/owncloud/ocis/v2/ocis-pkg/log"
	searchmsg "github.com/owncloud/ocis/v2/protogen/gen/ocis/messages/search/v0"
	searchsvc "github.com/owncloud/ocis/v2/protogen/gen/ocis/services/search/v0"
	"github.com/owncloud/ocis/v2/services/search/pkg/config"
	"github.com/owncloud/ocis/v2/services/search/pkg/content"
	"github.com/owncloud/ocis/v2/services/search/pkg/engine"
)

//go:generate mockery --name=Searcher

const (
	_spaceStateTrashed   = "trashed"
	_spaceTypeMountpoint = "mountpoint"
	_spaceTypePersonal   = "personal"
	_spaceTypeProject    = "project"
	_spaceTypeGrant      = "grant"
	_slowQueryDuration   = 500 * time.Millisecond
)

// Searcher is the interface to the SearchService
type Searcher interface {
	Search(ctx context.Context, req *searchsvc.SearchRequest) (*searchsvc.SearchResponse, error)
	IndexSpace(rID *provider.StorageSpaceId, uID *user.UserId) error
	TrashItem(rID *provider.ResourceId)
	UpsertItem(ref *provider.Reference, uID *user.UserId)
	RestoreItem(ref *provider.Reference, uID *user.UserId)
	MoveItem(ref *provider.Reference, uID *user.UserId)
}

// Service is responsible for indexing spaces and pass on a search
// to it's underlying engine.
type Service struct {
	logger          log.Logger
	gatewaySelector pool.Selectable[gateway.GatewayAPIClient]
	engine          engine.Engine
	extractor       content.Extractor
	secret          string
}

var errSkipSpace error

// NewService creates a new Provider instance.
func NewService(gatewaySelector pool.Selectable[gateway.GatewayAPIClient], eng engine.Engine, extractor content.Extractor, logger log.Logger, cfg *config.Config) *Service {
	var s = &Service{
		gatewaySelector: gatewaySelector,
		engine:          eng,
		secret:          cfg.MachineAuthAPIKey,
		logger:          logger,
		extractor:       extractor,
	}

	return s
}

// Search processes a search request and passes it down to the engine.
func (s *Service) Search(ctx context.Context, req *searchsvc.SearchRequest) (*searchsvc.SearchResponse, error) {
	s.logger.Debug().Str("query", req.Query).Msg("performing a search")

	gatewayClient, err := s.gatewaySelector.Next()
	if err != nil {
		return nil, err
	}
	currentUser := revactx.ContextMustGetUser(ctx)

	// Extract scope from query if set
	query, scope := ParseScope(req.Query)
	if query == "" {
		return nil, errtypes.BadRequest("empty query provided")
	}
	req.Query = query
	if len(scope) > 0 {
		// if req.Ref != nil {
		// 	return nil, errtypes.BadRequest("cannot scope a search that is limited to a resource")
		// }
		scopeRef, err := extractScope(scope)
		if err != nil {
			return nil, err
		}
		// Stat the scope to get the resource id
		statRes, err := gatewayClient.Stat(ctx, &provider.StatRequest{
			Ref:       scopeRef,
			FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"space"}},
		})
		if err != nil {
			return nil, err
		}
		// GetPath the scope to get the full path in the space
		gpRes, err := gatewayClient.GetPath(ctx, &provider.GetPathRequest{
			ResourceId: statRes.GetInfo().GetId(),
		})
		if err != nil {
			return nil, err
		}

		req.Ref = &searchmsg.Reference{
			ResourceId: &searchmsg.ResourceID{
				StorageId: statRes.GetInfo().GetSpace().GetRoot().GetStorageId(),
				SpaceId:   statRes.GetInfo().GetSpace().GetRoot().GetSpaceId(),
				OpaqueId:  statRes.GetInfo().GetSpace().GetRoot().GetOpaqueId(),
			},
			Path: gpRes.Path,
		}
	}
	filters := []*provider.ListStorageSpacesRequest_Filter{
		{
			Type: provider.ListStorageSpacesRequest_Filter_TYPE_USER,
			Term: &provider.ListStorageSpacesRequest_Filter_User{User: currentUser.GetId()},
		},
		{
			Type: provider.ListStorageSpacesRequest_Filter_TYPE_SPACE_TYPE,
			Term: &provider.ListStorageSpacesRequest_Filter_SpaceType{SpaceType: "+grant"},
		},
	}

	// Get the spaces to search
	spaces := []*provider.StorageSpace{}
	listSpacesRes, err := gatewayClient.ListStorageSpaces(ctx, &provider.ListStorageSpacesRequest{Filters: filters})
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to list the user's storage spaces")
		return nil, err
	}
	for _, space := range listSpacesRes.StorageSpaces {
		if utils.ReadPlainFromOpaque(space.Opaque, "trashed") == _spaceStateTrashed {
			// Do not consider disabled spaces
			continue
		}
		if space.SpaceType != "mountpoint" && req.Ref != nil && (req.Ref.GetResourceId().GetSpaceId() != space.Root.GetSpaceId()) {
			// Do not search (non-mountpoint) spaces that do not match the given scope (if a scope is set)
			// We still need the mountpoint in order to map the result paths to the according share
			continue
		}
		spaces = append(spaces, space)
	}

	mountpointMap := map[string]string{}
	for _, space := range spaces {
		if space.SpaceType != _spaceTypeMountpoint {
			continue
		}
		opaqueMap := sdk.DecodeOpaqueMap(space.Opaque)
		grantSpaceID := storagespace.FormatResourceID(provider.ResourceId{
			StorageId: opaqueMap["grantStorageID"],
			SpaceId:   opaqueMap["grantSpaceID"],
			OpaqueId:  opaqueMap["grantOpaqueID"],
		})
		mountpointMap[grantSpaceID] = space.Id.OpaqueId
	}

	matches := matchArray{}
	total := int32(0)

	errg, ctx := errgroup.WithContext(ctx)
	work := make(chan *provider.StorageSpace, len(spaces))
	results := make(chan *searchsvc.SearchIndexResponse, len(spaces))

	// Distribute work
	errg.Go(func() error {
		defer close(work)
		for _, space := range spaces {
			select {
			case work <- space:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})

	// Spawn workers that'll concurrently work the queue
	numWorkers := 20
	if len(spaces) < numWorkers {
		numWorkers = len(spaces)
	}
	for i := 0; i < numWorkers; i++ {
		errg.Go(func() error {
			for space := range work {
				res, err := s.searchIndex(ctx, req, space, mountpointMap[space.Id.OpaqueId])
				if err != nil && err != errSkipSpace {
					return err
				}

				select {
				case results <- res:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}

	// Wait for things to settle down, then close results chan
	go func() {
		_ = errg.Wait() // error is checked later
		close(results)
	}()

	responses := make([]*searchsvc.SearchIndexResponse, len(spaces))
	i := 0
	for r := range results {
		responses[i] = r
		i++
	}

	if err := errg.Wait(); err != nil {
		return nil, err
	}

	for _, res := range responses {
		if res == nil {
			continue
		}
		total += res.TotalMatches
		for _, match := range res.Matches {
			matches = append(matches, match)
		}
	}

	// compile one sorted list of matches from all spaces and apply the limit if needed
	sort.Sort(matches)
	limit := req.PageSize
	if limit == 0 {
		limit = 200
	}
	if int32(len(matches)) > limit && limit != -1 {
		matches = matches[0:limit]
	}

	return &searchsvc.SearchResponse{
		Matches:      matches,
		TotalMatches: total,
	}, nil
}

func (s *Service) searchIndex(ctx context.Context, req *searchsvc.SearchRequest, space *provider.StorageSpace, mountpointID string) (*searchsvc.SearchIndexResponse, error) {
	if req.Ref != nil &&
		(req.Ref.ResourceId.StorageId != space.Root.StorageId ||
			req.Ref.ResourceId.SpaceId != space.Root.SpaceId) {
		return nil, errSkipSpace
	}

	searchRootID := &searchmsg.ResourceID{
		StorageId: space.Root.StorageId,
		SpaceId:   space.Root.SpaceId,
		OpaqueId:  space.Root.OpaqueId,
	}

	var (
		mountpointRootID *searchmsg.ResourceID
		rootName         string
		permissions      *provider.ResourcePermissions
	)
	mountpointPrefix := ""
	searchPathPrefix := req.Ref.GetPath()
	switch space.SpaceType {
	case _spaceTypeMountpoint:
		return nil, errSkipSpace // mountpoint spaces are only "links" to the shared spaces. we have to search the shared "grant" space instead
	case _spaceTypeGrant:
		// In case of grant spaces we search the root of the outer space and translate the paths to the according mountpoint
		searchRootID.OpaqueId = space.Root.SpaceId
		if mountpointID == "" {
			s.logger.Warn().Interface("space", space).Msg("could not find mountpoint space for grant space")
			return nil, errSkipSpace
		}

		gatewayClient, err := s.gatewaySelector.Next()
		if err != nil {
			return nil, err
		}

		var ownerCtx context.Context
		if space.Owner.Id.Type == user.UserType_USER_TYPE_SPACE_OWNER {
			// We can't impersonate SPACE_OWNER users and have to fall back to using the user auth instead,
			// which will not resolve the absolute path of the share in the space but only the part the user
			// is allowed to see.
			// In the future this problem can be solved using service accounts.
			ownerCtx = ctx
		} else {
			ownerCtx, err = getAuthContext(&user.User{Id: space.Owner.Id}, s.gatewaySelector, s.secret, s.logger)
			if err != nil {
				return nil, err
			}
		}

		gpRes, err := gatewayClient.GetPath(ownerCtx, &provider.GetPathRequest{
			ResourceId: space.Root,
		})
		if err != nil {
			s.logger.Error().Err(err).Str("space", space.Id.OpaqueId).Msg("failed to get path for grant space root")
			return nil, errSkipSpace
		}
		if gpRes.Status.Code != rpcv1beta1.Code_CODE_OK {
			s.logger.Error().Interface("status", gpRes.Status).Str("space", space.Id.OpaqueId).Msg("failed to get path for grant space root")
			return nil, errSkipSpace
		}
		mountpointPrefix = utils.MakeRelativePath(gpRes.Path)
		if searchPathPrefix == "" {
			searchPathPrefix = mountpointPrefix
		}
		sid, spid, oid, err := storagespace.SplitID(mountpointID)
		if err != nil {
			s.logger.Error().Err(err).Str("space", space.Id.OpaqueId).Str("mountpointId", mountpointID).Msg("invalid mountpoint space id")
			return nil, errSkipSpace
		}
		mountpointRootID = &searchmsg.ResourceID{
			StorageId: sid,
			SpaceId:   spid,
			OpaqueId:  oid,
		}
		rootName = space.GetRootInfo().GetPath()
		permissions = space.GetRootInfo().GetPermissionSet()
		s.logger.Debug().Interface("grantSpace", space).Interface("mountpointRootId", mountpointRootID).Msg("searching a grant")
	case _spaceTypePersonal, _spaceTypeProject:
		permissions = space.GetRootInfo().GetPermissionSet()
	}

	searchRequest := &searchsvc.SearchIndexRequest{
		Query: req.Query,
		Ref: &searchmsg.Reference{
			ResourceId: searchRootID,
			Path:       searchPathPrefix,
		},
		PageSize: req.PageSize,
	}
	start := time.Now()
	res, err := s.engine.Search(ctx, searchRequest)
	duration := time.Since(start)
	if err != nil {
		s.logger.Error().Err(err).Str("duration", fmt.Sprint(duration)).Str("space", space.Id.OpaqueId).Msg("failed to search the index")
		return nil, err
	}
	if duration > _slowQueryDuration {
		s.logger.Info().Interface("searchRequest", searchRequest).Str("duration", fmt.Sprint(duration)).Str("space", space.Id.OpaqueId).Int("hits", len(res.Matches)).Msg("slow space search")
	} else {
		s.logger.Debug().Interface("searchRequest", searchRequest).Str("duration", fmt.Sprint(duration)).Str("space", space.Id.OpaqueId).Int("hits", len(res.Matches)).Msg("space search done")
	}

	var matches []*searchmsg.Match

	for _, match := range res.Matches {
		if mountpointPrefix != "" {
			match.Entity.Ref.Path = utils.MakeRelativePath(strings.TrimPrefix(match.Entity.Ref.Path, mountpointPrefix))
		}
		if mountpointRootID != nil {
			match.Entity.Ref.ResourceId = mountpointRootID
		}
		match.Entity.ShareRootName = rootName

		isShared := match.GetEntity().GetRef().GetResourceId().GetSpaceId() == utils.ShareStorageSpaceID
		isMountpoint := isShared && match.GetEntity().GetRef().GetPath() == "."
		isDir := match.GetEntity().GetMimeType() == "httpd/unix-directory"
		match.Entity.Permissions = convertToWebDAVPermissions(isShared, isMountpoint, isDir, permissions)

		if req.Ref != nil && searchPathPrefix == "/"+match.Entity.Name {
			continue
		}

		matches = append(matches, match)
	}

	res.Matches = matches

	return res, nil
}

// IndexSpace (re)indexes all resources of a given space.
func (s *Service) IndexSpace(spaceID *provider.StorageSpaceId, uID *user.UserId) error {
	ownerCtx, err := getAuthContext(&user.User{Id: uID}, s.gatewaySelector, s.secret, s.logger)
	if err != nil {
		return err
	}

	rootID, err := storagespace.ParseID(spaceID.OpaqueId)
	if err != nil {
		s.logger.Error().Err(err).Msg("invalid space id")
		return err
	}
	if rootID.StorageId == "" || rootID.SpaceId == "" {
		s.logger.Error().Err(err).Msg("invalid space id")
		return fmt.Errorf("invalid space id")
	}
	rootID.OpaqueId = rootID.SpaceId

	w := walker.NewWalker(s.gatewaySelector)
	err = w.Walk(ownerCtx, &rootID, func(wd string, info *provider.ResourceInfo, err error) error {
		if err != nil {
			s.logger.Error().Err(err).Msg("error walking the tree")
			return err
		}

		if info == nil {
			return nil
		}

		ref := &provider.Reference{
			Path:       utils.MakeRelativePath(filepath.Join(wd, info.Path)),
			ResourceId: &rootID,
		}
		s.logger.Debug().Str("path", ref.Path).Msg("Walking tree")

		searchRes, err := s.engine.Search(ownerCtx, &searchsvc.SearchIndexRequest{
			Query: "+ID:" + storagespace.FormatResourceID(*info.Id) + ` +Mtime:>="` + utils.TSToTime(info.Mtime).Format(time.RFC3339Nano) + `"`,
		})

		if err == nil && len(searchRes.Matches) >= 1 {
			if info.Type == provider.ResourceType_RESOURCE_TYPE_CONTAINER {
				s.logger.Debug().Str("path", ref.Path).Msg("subtree hasn't changed. Skipping.")
				return filepath.SkipDir
			}
			s.logger.Debug().Str("path", ref.Path).Msg("element hasn't changed. Skipping.")
			return nil
		}

		s.UpsertItem(ref, uID)

		return nil
	})

	if err != nil {
		return err
	}

	logDocCount(s.engine, s.logger)

	return nil
}

// TrashItem marks the item as deleted.
func (s *Service) TrashItem(rID *provider.ResourceId) {
	err := s.engine.Delete(storagespace.FormatResourceID(*rID))
	if err != nil {
		s.logger.Error().Err(err).Interface("Id", rID).Msg("failed to remove item from index")
	}
}

// UpsertItem indexes or stores Resource data fields.
func (s *Service) UpsertItem(ref *provider.Reference, uID *user.UserId) {
	ctx, stat, path := s.resInfo(uID, ref)
	if ctx == nil || stat == nil || path == "" {
		return
	}

	doc, err := s.extractor.Extract(ctx, stat.Info)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to extract resource content")
		return
	}

	r := engine.Resource{
		ID: storagespace.FormatResourceID(*stat.Info.Id),
		RootID: storagespace.FormatResourceID(provider.ResourceId{
			StorageId: stat.Info.Id.StorageId,
			OpaqueId:  stat.Info.Id.SpaceId,
			SpaceId:   stat.Info.Id.SpaceId,
		}),
		Path:     utils.MakeRelativePath(path),
		Type:     uint64(stat.Info.Type),
		Document: doc,
	}
	r.Hidden = strings.HasPrefix(r.Path, ".")

	if parentID := stat.GetInfo().GetParentId(); parentID != nil {
		r.ParentID = storagespace.FormatResourceID(*parentID)
	}

	if err = s.engine.Upsert(r.ID, r); err != nil {
		s.logger.Error().Err(err).Msg("error adding updating the resource in the index")
	} else {
		logDocCount(s.engine, s.logger)
	}
}

// RestoreItem makes the item available again.
func (s *Service) RestoreItem(ref *provider.Reference, uID *user.UserId) {
	ctx, stat, path := s.resInfo(uID, ref)
	if ctx == nil || stat == nil || path == "" {
		return
	}

	if err := s.engine.Restore(storagespace.FormatResourceID(*stat.Info.Id)); err != nil {
		s.logger.Error().Err(err).Msg("failed to restore the changed resource in the index")
	}
}

// MoveItem updates the resource location and all of its necessary fields.
func (s *Service) MoveItem(ref *provider.Reference, uID *user.UserId) {
	ctx, stat, path := s.resInfo(uID, ref)
	if ctx == nil || stat == nil || path == "" {
		return
	}

	if err := s.engine.Move(storagespace.FormatResourceID(*stat.GetInfo().GetId()), storagespace.FormatResourceID(*stat.GetInfo().GetParentId()), path); err != nil {
		s.logger.Error().Err(err).Msg("failed to move the changed resource in the index")
	}
}

func (s *Service) resInfo(uID *user.UserId, ref *provider.Reference) (context.Context, *provider.StatResponse, string) {
	ownerCtx, err := getAuthContext(&user.User{Id: uID}, s.gatewaySelector, s.secret, s.logger)
	if err != nil {
		return nil, nil, ""
	}

	statRes, err := statResource(ownerCtx, ref, s.gatewaySelector, s.logger)
	if err != nil {
		return nil, nil, ""
	}

	r, err := ResolveReference(ownerCtx, ref, statRes.GetInfo(), s.gatewaySelector)
	if err != nil {
		return nil, nil, ""
	}

	return ownerCtx, statRes, r.GetPath()
}
