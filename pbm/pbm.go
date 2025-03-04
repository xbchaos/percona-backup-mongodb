package pbm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"github.com/percona/percona-backup-mongodb/pbm/compress"
	"github.com/percona/percona-backup-mongodb/pbm/log"
)

const (
	// DB is a name of the PBM database
	DB = "admin"
	// LogCollection is the name of the mongo collection that contains PBM logs
	LogCollection = "pbmLog"
	// ConfigCollection is the name of the mongo collection that contains PBM configs
	ConfigCollection = "pbmConfig"
	// LockCollection is the name of the mongo collection that is used
	// by agents to coordinate mutually exclusive operations (e.g. backup/restore)
	LockCollection = "pbmLock"
	// LockOpCollection is the name of the mongo collection that is used
	// by agents to coordinate operations that don't need to be
	// mutually exclusive to other operation types (e.g. backup-delete)
	LockOpCollection = "pbmLockOp"
	// BcpCollection is a collection for backups metadata
	BcpCollection = "pbmBackups"
	// RestoresCollection is a collection for restores metadata
	RestoresCollection = "pbmRestores"
	// CmdStreamCollection is the name of the mongo collection that contains backup/restore commands stream
	CmdStreamCollection = "pbmCmd"
	// PITRChunksCollection contains index metadata of PITR chunks
	PITRChunksCollection = "pbmPITRChunks"
	// PBMOpLogCollection contains log of acquired locks (hence run ops)
	PBMOpLogCollection = "pbmOpLog"
	// AgentsStatusCollection is an agents registry with its status/health checks
	AgentsStatusCollection = "pbmAgents"

	// MetadataFileSuffix is a suffix for the metadata file on a storage
	MetadataFileSuffix = ".pbm.json"
)

// ErrNotFound - object not found
var ErrNotFound = errors.New("not found")

// Command represents actions that could be done on behalf of the client by the agents
type Command string

const (
	CmdUndefined    Command = ""
	CmdBackup       Command = "backup"
	CmdRestore      Command = "restore"
	CmdReplay       Command = "replay"
	CmdCancelBackup Command = "cancelBackup"
	CmdResync       Command = "resync"
	CmdPITR         Command = "pitr"
	CmdPITRestore   Command = "pitrestore"
	CmdDeleteBackup Command = "delete"
	CmdDeletePITR   Command = "deletePitr"
	CmdCleanup      Command = "cleanup"
)

func (c Command) String() string {
	switch c {
	case CmdBackup:
		return "Snapshot backup"
	case CmdRestore:
		return "Snapshot restore"
	case CmdReplay:
		return "Oplog replay"
	case CmdCancelBackup:
		return "Backup cancellation"
	case CmdResync:
		return "Resync storage"
	case CmdPITR:
		return "PITR incremental backup"
	case CmdPITRestore:
		return "PITR restore"
	case CmdDeleteBackup:
		return "Delete"
	case CmdDeletePITR:
		return "Delete PITR chunks"
	case CmdCleanup:
		return "Cleanup backups and PITR chunks"
	default:
		return "Undefined"
	}
}

type OPID primitive.ObjectID

type Cmd struct {
	Cmd        Command          `bson:"cmd"`
	Backup     *BackupCmd       `bson:"backup,omitempty"`
	Restore    *RestoreCmd      `bson:"restore,omitempty"`
	Replay     *ReplayCmd       `bson:"replay,omitempty"`
	PITRestore *PITRestoreCmd   `bson:"pitrestore,omitempty"`
	Delete     *DeleteBackupCmd `bson:"delete,omitempty"`
	DeletePITR *DeletePITRCmd   `bson:"deletePitr,omitempty"`
	Cleanup    *CleanupCmd      `bson:"cleanup,omitempty"`
	TS         int64            `bson:"ts"`
	OPID       OPID             `bson:"-"`
}

func OPIDfromStr(s string) (OPID, error) {
	o, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		return OPID(primitive.NilObjectID), err
	}
	return OPID(o), nil
}

func NilOPID() OPID { return OPID(primitive.NilObjectID) }

func (o OPID) String() string {
	return primitive.ObjectID(o).Hex()
}

func (o OPID) Obj() primitive.ObjectID {
	return primitive.ObjectID(o)
}

func (c Cmd) String() string {
	var buf bytes.Buffer

	buf.WriteString(string(c.Cmd))
	switch c.Cmd {
	case CmdBackup:
		buf.WriteString(" [")
		buf.WriteString(c.Backup.String())
		buf.WriteString("]")
	case CmdRestore:
		buf.WriteString(" [")
		buf.WriteString(c.Restore.String())
		buf.WriteString("]")
	case CmdPITRestore:
		buf.WriteString(" [")
		buf.WriteString(c.PITRestore.String())
		buf.WriteString("]")
	}
	buf.WriteString(" <ts: ")
	buf.WriteString(strconv.FormatInt(c.TS, 10))
	buf.WriteString(">")
	return buf.String()
}

type BackupCmd struct {
	Type             BackupType               `bson:"type"`
	IncrBase         bool                     `bson:"base"`
	Name             string                   `bson:"name"`
	Namespaces       []string                 `bson:"nss,omitempty"`
	Compression      compress.CompressionType `bson:"compression"`
	CompressionLevel *int                     `bson:"level,omitempty"`
}

func (b BackupCmd) String() string {
	var level string
	if b.CompressionLevel == nil {
		level = "default"
	} else {
		level = strconv.Itoa(*b.CompressionLevel)
	}
	return fmt.Sprintf("name: %s, compression: %s (level: %s)", b.Name, b.Compression, level)
}

type RestoreCmd struct {
	Name       string            `bson:"name"`
	BackupName string            `bson:"backupName"`
	Namespaces []string          `bson:"nss,omitempty"`
	RSMap      map[string]string `bson:"rsMap,omitempty"`
}

func (r RestoreCmd) String() string {
	return fmt.Sprintf("name: %s, backup name: %s", r.Name, r.BackupName)
}

type ReplayCmd struct {
	Name  string              `bson:"name"`
	Start primitive.Timestamp `bson:"start,omitempty"`
	End   primitive.Timestamp `bson:"end,omitempty"`
	RSMap map[string]string   `bson:"rsMap,omitempty"`
}

func (c ReplayCmd) String() string {
	return fmt.Sprintf("name: %s, time: %d - %d", c.Name, c.Start, c.End)
}

type PITRestoreCmd struct {
	Name       string            `bson:"name"`
	TS         int64             `bson:"ts"`
	I          int64             `bson:"i"`
	Bcp        string            `bson:"bcp"`
	Namespaces []string          `bson:"nss,omitempty"`
	RSMap      map[string]string `bson:"rsMap,omitempty"`
}

func (p PITRestoreCmd) String() string {
	if p.Bcp != "" {
		return fmt.Sprintf("name: %s, point-in-time ts: %d, base-snapshot: %s", p.Name, p.TS, p.Bcp)
	}
	return fmt.Sprintf("name: %s, point-in-time ts: %d", p.Name, p.TS)
}

type DeleteBackupCmd struct {
	Backup    string `bson:"backup"`
	OlderThan int64  `bson:"olderthan"`
}

type DeletePITRCmd struct {
	OlderThan int64 `bson:"olderthan"`
}

type CleanupCmd struct {
	OlderThan primitive.Timestamp `bson:"olderThan"`
}

func (d DeleteBackupCmd) String() string {
	return fmt.Sprintf("backup: %s, older than: %d", d.Backup, d.OlderThan)
}

const (
	PITRcheckRange       = time.Second * 15
	AgentsStatCheckRange = time.Second * 5
)

var (
	WaitActionStart = time.Second * 15
	WaitBackupStart = WaitActionStart + PITRcheckRange*12/10
)

// OpLog represents log of started operation.
// Operation progress can be get from logs by OPID.
// Basically it is a log of all ever taken locks. With the
// uniqueness by rs + opid
type OpLog struct {
	LockHeader `bson:",inline" json:",inline"`
}

type PBM struct {
	Conn *mongo.Client
	log  *log.Logger
	ctx  context.Context
}

// New creates a new PBM object.
// In the sharded cluster both agents and ctls should have a connection to ConfigServer replica set in order to communicate via PBM collections.
// If agent's or ctl's local node is not a member of ConfigServer, after discovering current topology connection will be established to ConfigServer.
func New(ctx context.Context, uri, appName string) (*PBM, error) {
	uri = "mongodb://" + strings.Replace(uri, "mongodb://", "", 1)

	client, err := connect(ctx, uri, appName)
	if err != nil {
		return nil, errors.Wrap(err, "create mongo connection")
	}

	pbm := &PBM{
		Conn: client,
		ctx:  ctx,
	}
	inf, err := pbm.GetNodeInfo()
	if err != nil {
		return nil, errors.Wrap(err, "get topology")
	}

	if !inf.IsSharded() || inf.ReplsetRole() == RoleConfigSrv {
		return pbm, errors.Wrap(pbm.setupNewDB(), "setup a new backups db")
	}

	csvr, err := ConfSvrConn(ctx, client)
	if err != nil {
		return nil, errors.Wrap(err, "get config server connection URI")
	}
	// no need in this connection anymore, we need a new one with the ConfigServer
	err = client.Disconnect(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "disconnect old client")
	}

	chost := strings.Split(csvr, "/")
	if len(chost) < 2 {
		return nil, errors.Wrapf(err, "define config server connection URI from %s", csvr)
	}

	curi, err := url.Parse(uri)
	if err != nil {
		return nil, errors.Wrapf(err, "parse mongo-uri '%s'", uri)
	}

	// Preserving the `replicaSet` parameter will cause an error while connecting to the ConfigServer (mismatched replicaset names)
	query := curi.Query()
	query.Del("replicaSet")
	curi.RawQuery = query.Encode()
	curi.Host = chost[1]
	pbm.Conn, err = connect(ctx, curi.String(), appName)
	if err != nil {
		return nil, errors.Wrapf(err, "create mongo connection to configsvr with connection string '%s'", curi)
	}

	return pbm, errors.Wrap(pbm.setupNewDB(), "setup a new backups db")
}

func (p *PBM) InitLogger(rs, node string) {
	p.log = log.New(p.Conn.Database(DB).Collection(LogCollection), rs, node)
}

func (p *PBM) Logger() *log.Logger {
	return p.log
}

const (
	cmdCollectionSizeBytes      = 1 << 20  // 1Mb
	pbmOplogCollectionSizeBytes = 10 << 20 // 10Mb
	logsCollectionSizeBytes     = 50 << 20 // 50Mb
)

// setup a new DB for PBM
func (p *PBM) setupNewDB() error {
	err := p.Conn.Database(DB).RunCommand(
		p.ctx,
		bson.D{{"create", CmdStreamCollection}, {"capped", true}, {"size", cmdCollectionSizeBytes}},
	).Err()
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrap(err, "ensure cmd collection")
	}

	err = p.Conn.Database(DB).RunCommand(
		p.ctx,
		bson.D{{"create", LogCollection}, {"capped", true}, {"size", logsCollectionSizeBytes}},
	).Err()
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrap(err, "ensure log collection")
	}

	err = p.Conn.Database(DB).RunCommand(
		p.ctx,
		bson.D{{"create", LockCollection}},
	).Err()
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrap(err, "ensure lock collection")
	}

	// create indexes for the lock collections
	_, err = p.Conn.Database(DB).Collection(LockCollection).Indexes().CreateOne(
		p.ctx,
		mongo.IndexModel{
			Keys: bson.D{{"replset", 1}},
			Options: options.Index().
				SetUnique(true).
				SetSparse(true),
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrapf(err, "ensure lock index on %s", LockCollection)
	}
	_, err = p.Conn.Database(DB).Collection(LockOpCollection).Indexes().CreateOne(
		p.ctx,
		mongo.IndexModel{
			Keys: bson.D{{"replset", 1}, {"type", 1}},
			Options: options.Index().
				SetUnique(true).
				SetSparse(true),
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrapf(err, "ensure lock index on %s", LockOpCollection)
	}

	err = p.Conn.Database(DB).RunCommand(
		p.ctx,
		bson.D{{"create", PBMOpLogCollection}, {"capped", true}, {"size", pbmOplogCollectionSizeBytes}},
	).Err()
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrap(err, "ensure log collection")
	}
	_, err = p.Conn.Database(DB).Collection(PBMOpLogCollection).Indexes().CreateOne(
		p.ctx,
		mongo.IndexModel{
			Keys: bson.D{{"opid", 1}, {"replset", 1}},
			Options: options.Index().
				SetUnique(true).
				SetSparse(true),
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrapf(err, "ensure lock index on %s", LockOpCollection)
	}

	// create indexs for the pitr chunks
	_, err = p.Conn.Database(DB).Collection(PITRChunksCollection).Indexes().CreateMany(
		p.ctx,
		[]mongo.IndexModel{
			{
				Keys: bson.D{{"rs", 1}, {"start_ts", 1}, {"end_ts", 1}},
				Options: options.Index().
					SetUnique(true).
					SetSparse(true),
			},
			{
				Keys: bson.D{{"start_ts", 1}, {"end_ts", 1}},
			},
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return errors.Wrap(err, "ensure pitr chunks index")
	}

	_, err = p.Conn.Database(DB).Collection(BcpCollection).Indexes().CreateMany(
		p.ctx,
		[]mongo.IndexModel{
			{
				Keys: bson.D{{"name", 1}},
				Options: options.Index().
					SetUnique(true).
					SetSparse(true),
			},
			{
				Keys: bson.D{{"start_ts", 1}, {"status", 1}},
			},
		},
	)

	return err
}

func connect(ctx context.Context, uri, appName string) (*mongo.Client, error) {
	client, err := mongo.NewClient(
		options.Client().ApplyURI(uri).
			SetAppName(appName).
			SetReadPreference(readpref.Primary()).
			SetReadConcern(readconcern.Majority()).
			SetWriteConcern(writeconcern.New(writeconcern.WMajority())),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create mongo client")
	}
	err = client.Connect(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "mongo connect")
	}

	err = client.Ping(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "mongo ping")
	}

	return client, nil
}

type BackupType string

const (
	PhysicalBackup    BackupType = "physical"
	IncrementalBackup BackupType = "incremental"
	LogicalBackup     BackupType = "logical"
)

// BackupMeta is a backup's metadata
type BackupMeta struct {
	Type BackupType `bson:"type" json:"type"`
	OPID string     `bson:"opid" json:"opid"`
	Name string     `bson:"name" json:"name"`

	// SrcBackup is the source for the incremental backups. The souce might be
	// incremental as well.
	// Empty means this is a full backup (and a base for further incremental bcps).
	SrcBackup string `bson:"src_backup,omitempty" json:"src_backup,omitempty"`

	// ShardRemap is map of replset to shard names.
	// If shard name is different from replset name, it will be stored in the map.
	// If all shard names are the same as their replset names, the map is nil.
	ShardRemap map[string]string `bson:"shardRemap,omitempty" json:"shardRemap,omitempty"`

	Namespaces       []string                 `bson:"nss,omitempty" json:"nss,omitempty"`
	Replsets         []BackupReplset          `bson:"replsets" json:"replsets"`
	Compression      compress.CompressionType `bson:"compression" json:"compression"`
	Store            StorageConf              `bson:"store" json:"store"`
	Size             int64                    `bson:"size" json:"size"`
	MongoVersion     string                   `bson:"mongodb_version" json:"mongodb_version,omitempty"`
	FCV              string                   `bson:"fcv" json:"fcv"`
	StartTS          int64                    `bson:"start_ts" json:"start_ts"`
	LastTransitionTS int64                    `bson:"last_transition_ts" json:"last_transition_ts"`
	FirstWriteTS     primitive.Timestamp      `bson:"first_write_ts" json:"first_write_ts"`
	LastWriteTS      primitive.Timestamp      `bson:"last_write_ts" json:"last_write_ts"`
	Hb               primitive.Timestamp      `bson:"hb" json:"hb"`
	Status           Status                   `bson:"status" json:"status"`
	Conditions       []Condition              `bson:"conditions" json:"conditions"`
	Nomination       []BackupRsNomination     `bson:"n" json:"n"`
	Err              string                   `bson:"error,omitempty" json:"error,omitempty"`
	PBMVersion       string                   `bson:"pbm_version,omitempty" json:"pbm_version,omitempty"`
	BalancerStatus   BalancerMode             `bson:"balancer" json:"balancer"`
	runtimeError     error
}

func (b *BackupMeta) Error() error {
	switch {
	case b.runtimeError != nil:
		return b.runtimeError
	case b.Err != "":
		return errors.New(b.Err)
	default:
		return nil
	}
}

func (b *BackupMeta) SetRuntimeError(err error) {
	b.runtimeError = err
	b.Status = StatusError
}

// BackupRsNomination is used to choose (nominate and elect) nodes for the backup
// within a replica set
type BackupRsNomination struct {
	RS    string   `bson:"rs" json:"rs"`
	Nodes []string `bson:"n" json:"n"`
	Ack   string   `bson:"ack" json:"ack"`
}

type Condition struct {
	Timestamp int64  `bson:"timestamp" json:"timestamp"`
	Status    Status `bson:"status" json:"status"`
	Error     string `bson:"error,omitempty" json:"error,omitempty"`
}

type BackupReplset struct {
	Name             string              `bson:"name" json:"name"`
	Journal          []File              `bson:"journal,omitempty" json:"journal,omitempty"` // not used. left for backward compatibility
	Files            []File              `bson:"files,omitempty" json:"files,omitempty"`
	DumpName         string              `bson:"dump_name,omitempty" json:"backup_name,omitempty"`
	OplogName        string              `bson:"oplog_name,omitempty" json:"oplog_name,omitempty"`
	StartTS          int64               `bson:"start_ts" json:"start_ts"`
	Status           Status              `bson:"status" json:"status"`
	IsConfigSvr      *bool               `bson:"iscs,omitempty" json:"iscs,omitempty"`
	LastTransitionTS int64               `bson:"last_transition_ts" json:"last_transition_ts"`
	FirstWriteTS     primitive.Timestamp `bson:"first_write_ts" json:"first_write_ts"`
	LastWriteTS      primitive.Timestamp `bson:"last_write_ts" json:"last_write_ts"`
	Node             string              `bson:"node" json:"node"` // node that performed backup
	Error            string              `bson:"error,omitempty" json:"error,omitempty"`
	Conditions       []Condition         `bson:"conditions" json:"conditions"`
	MongodOpts       *MongodOpts         `bson:"mongod_opts,omitempty" json:"mongod_opts,omitempty"`
}

type File struct {
	Name    string      `bson:"filename" json:"filename"`
	Off     int64       `bson:"offset" json:"offset"` // offset for incremental backups
	Len     int64       `bson:"length" json:"length"` // length of chunk after the offset
	Size    int64       `bson:"fileSize" json:"fileSize"`
	StgSize int64       `bson:"stgSize" json:"stgSize"`
	Fmode   os.FileMode `bson:"fmode" json:"fmode"`
}

func (f File) String() string {
	if f.Off == 0 && f.Len == 0 {
		return f.Name
	}
	return fmt.Sprintf("%s [%d:%d]", f.Name, f.Off, f.Len)
}

func (f *File) WriteTo(w io.Writer) (int64, error) {
	fd, err := os.Open(f.Name)
	if err != nil {
		return 0, errors.Wrap(err, "open file for reading")
	}
	defer fd.Close()

	if f.Len == 0 && f.Off == 0 {
		return io.Copy(w, fd)
	}

	return io.Copy(w, io.NewSectionReader(fd, f.Off, f.Len))
}

// Status is a backup current status
type Status string

const (
	StatusInit  Status = "init"
	StatusReady Status = "ready"

	// for phys restore, to indicate shards have been stopped
	StatusDown Status = "down"

	StatusStarting   Status = "starting"
	StatusRunning    Status = "running"
	StatusDumpDone   Status = "dumpDone"
	StatusPartlyDone Status = "partlyDone"
	StatusDone       Status = "done"
	StatusCancelled  Status = "canceled"
	StatusError      Status = "error"
)

func (p *PBM) SetBackupMeta(m *BackupMeta) error {
	m.LastTransitionTS = m.StartTS
	m.Conditions = append(m.Conditions, Condition{
		Timestamp: m.StartTS,
		Status:    m.Status,
	})

	_, err := p.Conn.Database(DB).Collection(BcpCollection).InsertOne(p.ctx, m)

	return err
}

// RS returns the metadata of the replset with given name.
// It returns nil if no replset found.
func (b *BackupMeta) RS(name string) *BackupReplset {
	for _, rs := range b.Replsets {
		if rs.Name == name {
			return &rs
		}
	}
	return nil
}

func (p *PBM) ChangeBackupStateOPID(opid string, s Status, msg string) error {
	return p.changeBackupState(bson.D{{"opid", opid}}, s, msg)
}

func (p *PBM) ChangeBackupState(bcpName string, s Status, msg string) error {
	return p.changeBackupState(bson.D{{"name", bcpName}}, s, msg)
}

func (p *PBM) changeBackupState(clause bson.D, s Status, msg string) error {
	ts := time.Now().UTC().Unix()
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		clause,
		bson.D{
			{"$set", bson.M{"status": s}},
			{"$set", bson.M{"last_transition_ts": ts}},
			{"$set", bson.M{"error": msg}},
			{"$push", bson.M{"conditions": Condition{Timestamp: ts, Status: s, Error: msg}}},
		},
	)

	return err
}

func (p *PBM) BackupHB(bcpName string) error {
	ts, err := p.ClusterTime()
	if err != nil {
		return errors.Wrap(err, "read cluster time")
	}

	_, err = p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}},
		bson.D{
			{"$set", bson.M{"hb": ts}},
		},
	)

	return errors.Wrap(err, "write into db")
}

func (p *PBM) SetSrcBackup(bcpName, srcName string) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}},
		bson.D{
			{"$set", bson.M{"src_backup": srcName}},
		},
	)

	return err
}

func (p *PBM) SetFirstWrite(bcpName string, first primitive.Timestamp) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}},
		bson.D{
			{"$set", bson.M{"first_write_ts": first}},
		},
	)

	return err
}

func (p *PBM) SetLastWrite(bcpName string, last primitive.Timestamp) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}},
		bson.D{
			{"$set", bson.M{"last_write_ts": last}},
		},
	)

	return err
}

func (p *PBM) AddRSMeta(bcpName string, rs BackupReplset) error {
	rs.LastTransitionTS = rs.StartTS
	rs.Conditions = append(rs.Conditions, Condition{
		Timestamp: rs.StartTS,
		Status:    rs.Status,
	})
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}},
		bson.D{{"$addToSet", bson.M{"replsets": rs}}},
	)

	return err
}

func (p *PBM) ChangeRSState(bcpName string, rsName string, s Status, msg string) error {
	ts := time.Now().UTC().Unix()
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}, {"replsets.name", rsName}},
		bson.D{
			{"$set", bson.M{"replsets.$.status": s}},
			{"$set", bson.M{"replsets.$.last_transition_ts": ts}},
			{"$set", bson.M{"replsets.$.error": msg}},
			{"$push", bson.M{"replsets.$.conditions": Condition{Timestamp: ts, Status: s, Error: msg}}},
		},
	)

	return err
}

func (p *PBM) IncBackupSize(ctx context.Context, bcpName string, size int64) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(ctx,
		bson.D{{"name", bcpName}},
		bson.D{{"$inc", bson.M{"size": size}}})

	return err
}

func (p *PBM) RSSetPhyFiles(bcpName string, rsName string, rs *BackupReplset) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}, {"replsets.name", rsName}},
		bson.D{
			{"$set", bson.M{"replsets.$.files": rs.Files}},
			{"$set", bson.M{"replsets.$.journal": rs.Journal}},
		},
	)

	return err
}

func (p *PBM) SetRSLastWrite(bcpName string, rsName string, ts primitive.Timestamp) error {
	_, err := p.Conn.Database(DB).Collection(BcpCollection).UpdateOne(
		p.ctx,
		bson.D{{"name", bcpName}, {"replsets.name", rsName}},
		bson.D{
			{"$set", bson.M{"replsets.$.last_write_ts": ts}},
		},
	)

	return err
}

func (p *PBM) GetBackupMeta(name string) (*BackupMeta, error) {
	return p.getBackupMeta(bson.D{{"name", name}})
}

func (p *PBM) GetBackupByOPID(opid string) (*BackupMeta, error) {
	return p.getBackupMeta(bson.D{{"opid", opid}})
}

func (p *PBM) getBackupMeta(clause bson.D) (*BackupMeta, error) {
	res := p.Conn.Database(DB).Collection(BcpCollection).FindOne(p.ctx, clause)
	if res.Err() != nil {
		if res.Err() == mongo.ErrNoDocuments {
			return nil, ErrNotFound
		}
		return nil, errors.Wrap(res.Err(), "get")
	}

	b := &BackupMeta{}
	err := res.Decode(b)
	return b, errors.Wrap(err, "decode")
}

func (p *PBM) LastIncrementalBackup() (*BackupMeta, error) {
	return p.getRecentBackup(nil, nil, -1, bson.D{{"type", string(IncrementalBackup)}})
}

// GetLastBackup returns last successfully finished backup
// or nil if there is no such backup yet. If ts isn't nil it will
// search for the most recent backup that finished before specified timestamp
func (p *PBM) GetLastBackup(before *primitive.Timestamp) (*BackupMeta, error) {
	return p.getRecentBackup(nil, before, -1, bson.D{{"nss", nil}, {"type", string(LogicalBackup)}})
}

func (p *PBM) GetFirstBackup(after *primitive.Timestamp) (*BackupMeta, error) {
	return p.getRecentBackup(after, nil, 1, bson.D{{"nss", nil}, {"type", string(LogicalBackup)}})
}

func (p *PBM) getRecentBackup(after, before *primitive.Timestamp, sort int, opts bson.D) (*BackupMeta, error) {
	q := append(opts, bson.E{"status", StatusDone})
	if after != nil {
		q = append(q, bson.E{"last_write_ts", bson.M{"$gte": after}})
	}
	if before != nil {
		q = append(q, bson.E{"last_write_ts", bson.M{"$lte": before}})
	}

	res := p.Conn.Database(DB).Collection(BcpCollection).FindOne(
		p.ctx,
		q,
		options.FindOne().SetSort(bson.D{{"start_ts", sort}}),
	)
	if res.Err() != nil {
		if res.Err() == mongo.ErrNoDocuments {
			return nil, ErrNotFound
		}
		return nil, errors.Wrap(res.Err(), "get")
	}

	b := new(BackupMeta)
	err := res.Decode(b)
	return b, errors.Wrap(err, "decode")
}

func (p *PBM) BackupGetNext(backup *BackupMeta) (*BackupMeta, error) {
	res := p.Conn.Database(DB).Collection(BcpCollection).FindOne(
		p.ctx,
		bson.D{
			{"start_ts", bson.M{"$gt": backup.LastWriteTS.T}},
			{"status", StatusDone},
		},
	)

	if res.Err() != nil {
		if res.Err() == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, errors.Wrap(res.Err(), "get")
	}

	b := new(BackupMeta)
	err := res.Decode(b)
	return b, errors.Wrap(err, "decode")
}

func (p *PBM) BackupsList(limit int64) ([]BackupMeta, error) {
	cur, err := p.Conn.Database(DB).Collection(BcpCollection).Find(
		p.ctx,
		bson.M{},
		options.Find().SetLimit(limit).SetSort(bson.D{{"start_ts", -1}}),
	)
	if err != nil {
		return nil, errors.Wrap(err, "query mongo")
	}

	defer cur.Close(p.ctx)

	backups := []BackupMeta{}
	for cur.Next(p.ctx) {
		b := BackupMeta{}
		err := cur.Decode(&b)
		if err != nil {
			return nil, errors.Wrap(err, "message decode")
		}
		if b.Type == "" {
			b.Type = LogicalBackup
		}
		backups = append(backups, b)
	}

	return backups, cur.Err()
}

func (p *PBM) BackupsDoneList(after *primitive.Timestamp, limit int64, order int) ([]BackupMeta, error) {
	q := bson.D{{"status", StatusDone}}
	if after != nil {
		q = append(q, bson.E{"last_write_ts", bson.M{"$gte": after}})
	}

	cur, err := p.Conn.Database(DB).Collection(BcpCollection).Find(
		p.ctx,
		q,
		options.Find().SetLimit(limit).SetSort(bson.D{{"last_write_ts", order}}),
	)
	if err != nil {
		return nil, errors.Wrap(err, "query mongo")
	}

	defer cur.Close(p.ctx)

	backups := []BackupMeta{}
	for cur.Next(p.ctx) {
		b := BackupMeta{}
		err := cur.Decode(&b)
		if err != nil {
			return nil, errors.Wrap(err, "message decode")
		}
		backups = append(backups, b)
	}

	return backups, cur.Err()
}

// ClusterMembers returns list of replicasets current cluster consists of
// (shards + configserver). The list would consist of on rs if cluster is
// a non-sharded rs.
func (p *PBM) ClusterMembers() ([]Shard, error) {
	// it would be a config server in sharded cluster
	inf, err := p.GetNodeInfo()
	if err != nil {
		return nil, errors.Wrap(err, "define cluster state")
	}

	shards := []Shard{{
		RS:   inf.SetName,
		Host: inf.SetName + "/" + strings.Join(inf.Hosts, ","),
	}}
	if inf.IsSharded() {
		s, err := p.GetShards()
		if err != nil {
			return nil, errors.Wrap(err, "get shards")
		}
		shards = append(shards, s...)
	}

	return shards, nil
}

// GetShards gets list of shards
func (p *PBM) GetShards() ([]Shard, error) {
	cur, err := p.Conn.Database("config").Collection("shards").Find(p.ctx, bson.M{})
	if err != nil {
		return nil, errors.Wrap(err, "query mongo")
	}

	defer cur.Close(p.ctx)

	shards := []Shard{}
	for cur.Next(p.ctx) {
		s := Shard{}
		err := cur.Decode(&s)
		if err != nil {
			return nil, errors.Wrap(err, "message decode")
		}
		s.RS = s.ID
		// _id may differ from the rs name, so extract rs name from the host (format like "rs2/localhost:27017")
		// see https://jira.percona.com/browse/PBM-595
		h := strings.Split(s.Host, "/")
		if len(h) > 1 {
			s.RS = h[0]
		}
		shards = append(shards, s)
	}

	return shards, cur.Err()
}

// Context returns object context
func (p *PBM) Context() context.Context {
	return p.ctx
}

// GetNodeInfo returns mongo node info
func (p *PBM) GetNodeInfo() (*NodeInfo, error) {
	inf, err := GetNodeInfo(p.ctx, p.Conn)
	if err != nil {
		return nil, errors.Wrap(err, "get NodeInfo")
	}

	opts := struct {
		Parsed MongodOpts `bson:"parsed" json:"parsed"`
	}{}
	err = p.Conn.Database("admin").RunCommand(p.ctx, bson.D{{"getCmdLineOpts", 1}}).Decode(&opts)
	if err != nil {
		return nil, errors.Wrap(err, "get mongod options")
	}
	inf.opts = opts.Parsed

	return inf, nil
}

// GetNodeInfo returns mongo node info
func (p *PBM) GetFeatureCompatibilityVersion() (string, error) {
	return getFeatureCompatibilityVersion(p.ctx, p.Conn)
}

// ClusterTime returns mongo's current cluster time
func (p *PBM) ClusterTime() (primitive.Timestamp, error) {
	// Make a read to force the cluster timestamp update.
	// Otherwise, cluster timestamp could remain the same between node info reads, while in fact time has been moved forward.
	err := p.Conn.Database(DB).Collection(LockCollection).FindOne(p.ctx, bson.D{}).Err()
	if err != nil && err != mongo.ErrNoDocuments {
		return primitive.Timestamp{}, errors.Wrap(err, "void read")
	}

	inf, err := p.GetNodeInfo()
	if err != nil {
		return primitive.Timestamp{}, errors.Wrap(err, "get NodeInfo")
	}

	if inf.ClusterTime == nil {
		return primitive.Timestamp{}, errors.Wrap(err, "no clusterTime in response")
	}

	return inf.ClusterTime.ClusterTime, nil
}

func (p *PBM) LogGet(r *log.LogRequest, limit int64) (*log.Entries, error) {
	return log.Get(p.Conn.Database(DB).Collection(LogCollection), r, limit, false)
}

func (p *PBM) LogGetExactSeverity(r *log.LogRequest, limit int64) (*log.Entries, error) {
	return log.Get(p.Conn.Database(DB).Collection(LogCollection), r, limit, true)
}

// SetBalancerStatus sets balancer status
func (p *PBM) SetBalancerStatus(m BalancerMode) error {
	var cmd string

	switch m {
	case BalancerModeOn:
		cmd = "_configsvrBalancerStart"
	case BalancerModeOff:
		cmd = "_configsvrBalancerStop"
	default:
		return errors.Errorf("unknown mode %s", m)
	}

	err := p.Conn.Database("admin").RunCommand(p.ctx, bson.D{{cmd, 1}}).Err()
	if err != nil {
		return errors.Wrap(err, "run mongo command")
	}
	return nil
}

// GetBalancerStatus returns balancer status
func (p *PBM) GetBalancerStatus() (*BalancerStatus, error) {
	inf := &BalancerStatus{}
	err := p.Conn.Database("admin").RunCommand(p.ctx, bson.D{{"_configsvrBalancerStatus", 1}}).Decode(inf)
	if err != nil {
		return nil, errors.Wrap(err, "run mongo command")
	}
	return inf, nil
}

type Epoch primitive.Timestamp

func (p *PBM) GetEpoch() (Epoch, error) {
	c, err := p.GetConfig()
	if err != nil {
		return Epoch{}, errors.Wrap(err, "get config")
	}

	return Epoch(c.Epoch), nil
}

func (p *PBM) ResetEpoch() (Epoch, error) {
	ct, err := p.ClusterTime()
	if err != nil {
		return Epoch{}, errors.Wrap(err, "get cluster time")
	}
	_, err = p.Conn.Database(DB).Collection(ConfigCollection).UpdateOne(
		p.ctx,
		bson.D{},
		bson.M{"$set": bson.M{"epoch": ct}},
	)

	return Epoch(ct), err
}

func (e Epoch) TS() primitive.Timestamp {
	return primitive.Timestamp(e)
}

// CopyColl copy documents matching the given filter and return number of copied documents
func CopyColl(ctx context.Context, from, to *mongo.Collection, filter interface{}) (n int, err error) {
	cur, err := from.Find(ctx, filter)
	if err != nil {
		return 0, errors.Wrap(err, "create cursor")
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		_, err = to.InsertOne(ctx, cur.Current)
		if err != nil {
			return 0, errors.Wrap(err, "insert document")
		}
		n++
	}

	return n, nil
}

func BackupCursorName(s string) string {
	return strings.NewReplacer("-", "", ":", "").Replace(s)
}

func ConfSvrConn(ctx context.Context, cn *mongo.Client) (string, error) {
	csvr := struct {
		URI string `bson:"configsvrConnectionString"`
	}{}
	err := cn.Database("admin").Collection("system.version").
		FindOne(ctx, bson.D{{"_id", "shardIdentity"}}).Decode(&csvr)

	return csvr.URI, err
}
