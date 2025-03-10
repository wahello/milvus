package metastore

import (
	"context"

	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

//go:generate mockery --name=RootCoordCatalog
type RootCoordCatalog interface {
	CreateCollection(ctx context.Context, collectionInfo *model.Collection, ts typeutil.Timestamp) error
	GetCollectionByID(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) (*model.Collection, error)
	GetCollectionByName(ctx context.Context, collectionName string, ts typeutil.Timestamp) (*model.Collection, error)
	ListCollections(ctx context.Context, ts typeutil.Timestamp) (map[string]*model.Collection, error)
	CollectionExists(ctx context.Context, collectionID typeutil.UniqueID, ts typeutil.Timestamp) bool
	DropCollection(ctx context.Context, collectionInfo *model.Collection, ts typeutil.Timestamp) error
	AlterCollection(ctx context.Context, oldColl *model.Collection, newColl *model.Collection, alterType AlterType, ts typeutil.Timestamp) error

	CreatePartition(ctx context.Context, partition *model.Partition, ts typeutil.Timestamp) error
	DropPartition(ctx context.Context, collectionID typeutil.UniqueID, partitionID typeutil.UniqueID, ts typeutil.Timestamp) error
	AlterPartition(ctx context.Context, oldPart *model.Partition, newPart *model.Partition, alterType AlterType, ts typeutil.Timestamp) error

	CreateAlias(ctx context.Context, alias *model.Alias, ts typeutil.Timestamp) error
	DropAlias(ctx context.Context, alias string, ts typeutil.Timestamp) error
	AlterAlias(ctx context.Context, alias *model.Alias, ts typeutil.Timestamp) error
	ListAliases(ctx context.Context, ts typeutil.Timestamp) ([]*model.Alias, error)

	// GetCredential gets the credential info for the username, returns error if no credential exists for this username.
	GetCredential(ctx context.Context, username string) (*model.Credential, error)
	// CreateCredential creates credential by Username and EncryptedPassword in crediential. Please make sure credential.Username isn't empty before calling this API. Credentials already exists will be altered.
	CreateCredential(ctx context.Context, credential *model.Credential) error
	// AlterCredential does exactly the same as CreateCredential
	AlterCredential(ctx context.Context, credential *model.Credential) error
	// DropCredential removes the credential of this username
	DropCredential(ctx context.Context, username string) error
	// ListCredentials gets all usernames.
	ListCredentials(ctx context.Context) ([]string, error)

	// CreateRole creates role by the entity for the tenant. Please make sure the tenent and entity.Name aren't empty. Empty entity.Name may end up with deleting all roles
	// Returns common.IgnorableError if the role already existes
	CreateRole(ctx context.Context, tenant string, entity *milvuspb.RoleEntity) error
	// DropRole removes a role by name
	DropRole(ctx context.Context, tenant string, roleName string) error
	// AlterUserRole changes the role of a user for the tenant. Please make sure the userEntity.Name and roleEntity.Name aren't empty before calling this API.
	// Returns common.IgnorableError
	// - if user has the role when AddUserToRole
	// - if user doen't have the role when RemoveUserFromRole
	AlterUserRole(ctx context.Context, tenant string, userEntity *milvuspb.UserEntity, roleEntity *milvuspb.RoleEntity, operateType milvuspb.OperateUserRoleType) error
	// ListRole returns lists of RoleResults for the tenant
	// Returns all role results if entity is nill
	// Returns only role results if entity.Name is provided
	// Returns UserInfo inside each RoleResult if includeUserInfo is True
	ListRole(ctx context.Context, tenant string, entity *milvuspb.RoleEntity, includeUserInfo bool) ([]*milvuspb.RoleResult, error)
	// ListUser returns list of UserResults for the tenant
	// Returns all users if entity is nill
	// Returns the specific user if enitity is provided
	// Returns RoleInfo inside each UserResult if includeRoleInfo is True
	ListUser(ctx context.Context, tenant string, entity *milvuspb.UserEntity, includeRoleInfo bool) ([]*milvuspb.UserResult, error)
	// AlterGrant  grants or revokes a grant of a role to an object, according to the operateType.
	// Please make sure entity and operateType are valid before calling this API
	AlterGrant(ctx context.Context, tenant string, entity *milvuspb.GrantEntity, operateType milvuspb.OperatePrivilegeType) error
	// DeleteGrant deletes all the grant for a role.
	// Please make sure the role.Name isn't empty before call this API.
	DeleteGrant(ctx context.Context, tenant string, role *milvuspb.RoleEntity) error
	// ListGrant lists all grant infos accoording to entity for the tenant
	// Please make sure entity valid before calling this API
	ListGrant(ctx context.Context, tenant string, entity *milvuspb.GrantEntity) ([]*milvuspb.GrantEntity, error)
	ListPolicy(ctx context.Context, tenant string) ([]string, error)
	// List all user role pair in string for the tenant
	// For example []string{"user1/role1"}
	ListUserRole(ctx context.Context, tenant string) ([]string, error)

	Close()
}

type AlterType int32

const (
	ADD AlterType = iota
	DELETE
	MODIFY
)

func (t AlterType) String() string {
	switch t {
	case ADD:
		return "ADD"
	case DELETE:
		return "DELETE"
	case MODIFY:
		return "MODIFY"
	}
	return ""
}

type DataCoordCatalog interface {
	ListSegments(ctx context.Context) ([]*datapb.SegmentInfo, error)
	AddSegment(ctx context.Context, segment *datapb.SegmentInfo) error
	// TODO Remove this later, we should update flush segments info for each segment separately, so far we still need transaction
	AlterSegments(ctx context.Context, newSegments []*datapb.SegmentInfo) error
	// AlterSegmentsAndAddNewSegment for transaction
	AlterSegmentsAndAddNewSegment(ctx context.Context, segments []*datapb.SegmentInfo, newSegment *datapb.SegmentInfo) error
	AlterSegment(ctx context.Context, newSegment *datapb.SegmentInfo, oldSegment *datapb.SegmentInfo) error
	SaveDroppedSegmentsInBatch(ctx context.Context, segments []*datapb.SegmentInfo) error
	DropSegment(ctx context.Context, segment *datapb.SegmentInfo) error
	RevertAlterSegmentsAndAddNewSegment(ctx context.Context, segments []*datapb.SegmentInfo, removalSegment *datapb.SegmentInfo) error

	MarkChannelDeleted(ctx context.Context, channel string) error
	IsChannelDropped(ctx context.Context, channel string) bool
	DropChannel(ctx context.Context, channel string) error

	ListChannelCheckpoint(ctx context.Context) (map[string]*internalpb.MsgPosition, error)
	SaveChannelCheckpoint(ctx context.Context, vChannel string, pos *internalpb.MsgPosition) error
	DropChannelCheckpoint(ctx context.Context, vChannel string) error
}

type IndexCoordCatalog interface {
	CreateIndex(ctx context.Context, index *model.Index) error
	ListIndexes(ctx context.Context) ([]*model.Index, error)
	AlterIndex(ctx context.Context, newIndex *model.Index) error
	AlterIndexes(ctx context.Context, newIndexes []*model.Index) error
	DropIndex(ctx context.Context, collID, dropIdxID typeutil.UniqueID) error

	CreateSegmentIndex(ctx context.Context, segIdx *model.SegmentIndex) error
	ListSegmentIndexes(ctx context.Context) ([]*model.SegmentIndex, error)
	AlterSegmentIndex(ctx context.Context, newSegIndex *model.SegmentIndex) error
	AlterSegmentIndexes(ctx context.Context, newSegIdxes []*model.SegmentIndex) error
	DropSegmentIndex(ctx context.Context, collID, partID, segID, buildID typeutil.UniqueID) error
}

type QueryCoordCatalog interface {
	SaveCollection(info *querypb.CollectionLoadInfo) error
	SavePartition(info ...*querypb.PartitionLoadInfo) error
	SaveReplica(replica *querypb.Replica) error
	GetCollections() ([]*querypb.CollectionLoadInfo, error)
	GetPartitions() (map[int64][]*querypb.PartitionLoadInfo, error)
	GetReplicas() ([]*querypb.Replica, error)
	ReleaseCollection(id int64) error
	ReleasePartition(collection int64, partitions ...int64) error
	ReleaseReplicas(collectionID int64) error
	ReleaseReplica(collection, replica int64) error
}
