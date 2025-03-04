package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	slog "log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v2"

	"github.com/percona/percona-backup-mongodb/pbm"
	"github.com/percona/percona-backup-mongodb/pbm/compress"
	"github.com/percona/percona-backup-mongodb/pbm/log"
	"github.com/percona/percona-backup-mongodb/pbm/storage"
	"github.com/percona/percona-backup-mongodb/pbm/storage/s3"
	"github.com/percona/percona-backup-mongodb/version"
)

const (
	defaultRSdbpath   = "/data/db"
	defaultCSRSdbpath = "/data/configdb"

	mongofslock = "mongod.lock"

	defaultPort = 27017
)

type files struct {
	BcpName string
	Cmpr    compress.CompressionType
	Data    []pbm.File

	// dbpath to cut from destination if there is any (see PBM-1058)
	dbpath string
}

type PhysRestore struct {
	cn     *pbm.PBM
	node   *pbm.Node
	dbpath string
	// an ephemeral port to restart mongod on during the restore
	tmpPort int
	tmpConf *os.File
	rsConf  *pbm.RSConfig     // original replset config
	shards  map[string]string // original shards list on config server
	cfgConn string            // shardIdentity configsvrConnectionString
	startTS int64
	secOpts *pbm.MongodOptsSec

	name     string
	opid     string
	nodeInfo *pbm.NodeInfo
	stg      storage.Storage
	bcp      *pbm.BackupMeta
	files    []files

	confOpts pbm.RestoreConf

	mongod string // location of mongod used for internal restarts

	// path to files on a storage the node will sync its
	// state with the resto of the cluster
	syncPathNode     string
	syncPathNodeStat string
	syncPathRS       string
	syncPathCluster  string
	syncPathPeers    map[string]struct{}
	// Shards to participate in restore.
	// Only the restore leader would have this info.
	syncPathShards map[string]struct{}
	// Non-ConfigServer shards
	syncPathDataShards map[string]struct{}

	stopHB chan struct{}

	log *log.Event

	rsMap map[string]string
}

func NewPhysical(cn *pbm.PBM, node *pbm.Node, inf *pbm.NodeInfo, rsMap map[string]string) (*PhysRestore, error) {
	opts, err := node.GetOpts(nil)
	if err != nil {
		return nil, errors.Wrap(err, "get mongo options")
	}
	p := opts.Storage.DBpath
	if p == "" {
		switch inf.ReplsetRole() {
		case pbm.RoleConfigSrv:
			p = defaultCSRSdbpath
		default:
			p = defaultRSdbpath
		}
	}

	if opts.Net.Port == 0 {
		opts.Net.Port = defaultPort
	}

	rcf, err := node.GetRSconf()
	if err != nil {
		return nil, errors.Wrap(err, "get replset config")
	}

	var shards map[string]string
	var csvr string
	if inf.IsSharded() {
		ss, err := cn.GetShards()
		if err != nil {
			return nil, errors.WithMessage(err, "get shards")
		}

		shards = make(map[string]string)
		for _, s := range ss {
			shards[s.ID] = s.Host
		}

		if inf.ReplsetRole() != pbm.RoleConfigSrv {
			csvr, err = node.ConfSvrConn()
			if err != nil {
				return nil, errors.Wrap(err, "get configsvrConnectionString")
			}
		}
	}

	if inf.SetName == "" {
		return nil, errors.New("undefined replica set")
	}

	tmpPort, err := peekTmpPort(opts.Net.Port)
	if err != nil {
		return nil, errors.Wrap(err, "peek tmp port")
	}

	return &PhysRestore{
		cn:       cn,
		node:     node,
		dbpath:   p,
		rsConf:   rcf,
		shards:   shards,
		cfgConn:  csvr,
		nodeInfo: inf,
		tmpPort:  tmpPort,
		secOpts:  opts.Security,
		rsMap:    rsMap,
	}, nil
}

// peeks a random free port in a range [minPort, maxPort]
func peekTmpPort(current int) (int, error) {
	const (
		rng = 1111
		try = 150
	)

	rand.Seed(time.Now().UnixNano())

	for i := 0; i < try; i++ {
		p := current + rand.Intn(rng) + 1
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}

	return -1, errors.Errorf("can't find unused port in range [%d, %d]", current, current+rng)
}

// Close releases object resources.
// Should be run to avoid leaks.
func (r *PhysRestore) close(noerr, cleanup bool) {
	if r.tmpConf != nil {
		r.log.Debug("rm tmp conf")
		err := os.Remove(r.tmpConf.Name())
		if err != nil {
			r.log.Error("remove tmp config %s: %v", r.tmpConf.Name(), err)
		}
	}
	// clean-up internal mongod log only if there is no error
	if noerr {
		r.log.Debug("rm tmp logs")
		err := os.Remove(path.Join(r.dbpath, internalMongodLog))
		if err != nil {
			r.log.Error("remove tmp mongod logs %s: %v", path.Join(r.dbpath, internalMongodLog), err)
		}
	} else if cleanup { // clean-up dbpath on err if needed
		r.log.Debug("clean-up dbpath")
		err := removeAll(r.dbpath, r.log)
		if err != nil {
			r.log.Error("flush dbpath %s: %v", r.dbpath, err)
		}
	}
	if r.stopHB != nil {
		close(r.stopHB)
	}
}

func (r *PhysRestore) flush() error {
	r.log.Debug("shutdown server")
	rsStat, err := r.node.GetReplsetStatus()
	if err != nil {
		return errors.Wrap(err, "get replset status")
	}

	if r.nodeInfo.IsConfigSrv() {
		r.log.Debug("waiting for shards to shutdown")
		_, err := r.waitFiles(pbm.StatusDown, r.syncPathDataShards, false)
		if err != nil {
			return errors.Wrap(err, "wait for datashards to shutdown")
		}
	}

	for {
		inf, err := r.node.GetInfo()
		if err != nil {
			return errors.Wrap(err, "get node info")
		}
		// single-node replica set won't stepdown do secondary
		// so we have to shut it down despite of role
		if !inf.IsPrimary || len(rsStat.Members) == 1 {
			err = r.node.Shutdown()
			if err != nil &&
				strings.Contains(err.Error(), // wait a bit and let the node to stepdown
					"(ConflictingOperationInProgress) This node is already in the process of stepping down") {
				return errors.Wrap(err, "shutdown server")
			}
			break
		}
		r.log.Debug("waiting to became secondary")
		time.Sleep(time.Second * 1)
	}

	r.log.Debug("waiting for the node to shutdown")
	err = waitMgoShutdown(r.dbpath)
	if err != nil {
		return errors.Wrap(err, "shutdown")
	}

	if r.nodeInfo.IsPrimary {
		err = r.stg.Save(r.syncPathRS+"."+string(pbm.StatusDown),
			okStatus(), -1)
		if err != nil {
			return errors.Wrap(err, "write replset StatusDown")
		}
	}

	r.log.Debug("revome old data")
	err = removeAll(r.dbpath, r.log)
	if err != nil {
		return errors.Wrapf(err, "flush dbpath %s", r.dbpath)
	}

	return nil
}

func waitMgoShutdown(dbpath string) error {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for range tk.C {
		f, err := os.Stat(path.Join(dbpath, mongofslock))
		if err != nil {
			return errors.Wrapf(err, "check for lock file %s", path.Join(dbpath, mongofslock))
		}

		if f.Size() == 0 {
			return nil
		}
	}

	return nil
}

// toState moves cluster to the given restore state.
// All communication happens via files in the restore dir on storage.
//
//		Status "done" is a special case. If at least one node in the replset moved
//		to the "done", the replset is "partlyDone". And a replset is "done" if all
//		nodes moved to "done". For cluster success, all replsets must move either
//		to "done" or "partlyDone". Cluster is "partlyDone" if at least one replset
//		is "partlyDone".
//
//	  - Each node writes a file with the given state.
//	  - The replset leader (primary node) or every rs node, in case of status
//	    "done",  waits for files from all replica set nodes. And writes a status
//	    file for the replica set.
//	  - The cluster leader (primary node - on config server in case of sharded) or
//	    every node, in case of status "done",  waits for status files from all
//	    replica sets. And sets the status file for the cluster.
//	  - Each node in turn waits for the cluster status file and returns (move further)
//	    once it's observed.
//
// State structure on storage:
//
//		.pbm.restore/<restore-name>
//			rs.<rs-name>/
//				node.<node-name>.hb			// hearbeats. last beat ts inside.
//				node.<node-name>.<status>	// node's PBM status. Inside is the ts of the transition. In case of error, file contains an error text.
//				rs.<status>					// replicaset's PBM status. Inside is the ts of the transition. In case of error, file contains an error text.
//			cluster.hb						// hearbeats. last beat ts inside.
//			cluster.<status>				// cluster's PBM status. Inside is the ts of the transition. In case of error, file contains an error text.
//
//	 For example:
//
//	     2022-08-02T18:50:35.1889332Z
//	     ├── cluster.done
//	     ├── cluster.hb
//	     ├── cluster.running
//	     ├── cluster.starting
//	     ├── rs.rs1
//	     │   ├── node.rs101:27017.done
//	     │   ├── node.rs101:27017.hb
//	     │   ├── node.rs101:27017.running
//	     │   ├── node.rs101:27017.starting
//	     │   ├── node.rs102:27017.done
//	     │   ├── node.rs102:27017.hb
//	     │   ├── node.rs102:27017.running
//	     │   ├── node.rs102:27017.starting
//	     │   ├── node.rs103:27017.done
//	     │   ├── node.rs103:27017.hb
//	     │   ├── node.rs103:27017.running
//	     │   ├── node.rs103:27017.starting
//	     │   ├── rs.done
//	     │   ├── rs.hb
//	     │   ├── rs.running
//	     │   └── rs.starting
func (r *PhysRestore) toState(status pbm.Status) (rStatus pbm.Status, err error) {
	defer func() {
		if err != nil {
			if r.nodeInfo.IsPrimary && status != pbm.StatusDone {
				serr := r.stg.Save(r.syncPathRS+"."+string(pbm.StatusError),
					errStatus(err), -1)
				if serr != nil {
					r.log.Error("toState: write replset error state `%v`: %v", err, serr)
				}
			}
			if r.nodeInfo.IsClusterLeader() && status != pbm.StatusDone {
				serr := r.stg.Save(r.syncPathCluster+"."+string(pbm.StatusError),
					errStatus(err), -1)
				if serr != nil {
					r.log.Error("toState: write cluster error state `%v`: %v", err, serr)
				}
			}
		}
	}()

	r.log.Info("moving to state %s", status)

	err = r.stg.Save(r.syncPathNode+"."+string(status),
		okStatus(), -1)
	if err != nil {
		return pbm.StatusError, errors.Wrap(err, "write node state")
	}

	if r.nodeInfo.IsPrimary || status == pbm.StatusDone {
		r.log.Info("waiting for `%s` status in rs %v", status, r.syncPathPeers)
		cstat, err := r.waitFiles(status, copyMap(r.syncPathPeers), false)
		if err != nil {
			return pbm.StatusError, errors.Wrap(err, "wait for nodes in rs")
		}

		err = r.stg.Save(r.syncPathRS+"."+string(cstat),
			okStatus(), -1)
		if err != nil {
			return pbm.StatusError, errors.Wrap(err, "write replset state")
		}
	}

	if r.nodeInfo.IsClusterLeader() || status == pbm.StatusDone {
		r.log.Info("waiting for shards %v", r.syncPathShards)
		cstat, err := r.waitFiles(status, copyMap(r.syncPathShards), true)
		if err != nil {
			return pbm.StatusError, errors.Wrap(err, "wait for shards")
		}

		err = r.stg.Save(r.syncPathCluster+"."+string(cstat),
			okStatus(), -1)
		if err != nil {
			return pbm.StatusError, errors.Wrap(err, "write replset state")
		}
	}

	r.log.Info("waiting for cluster")
	cstat, err := r.waitFiles(status, map[string]struct{}{r.syncPathCluster: {}}, true)
	if err != nil {
		return pbm.StatusError, errors.Wrap(err, "wait for shards")
	}

	r.log.Debug("converged to state %s", cstat)

	return cstat, nil
}

func errStatus(err error) io.Reader {
	return bytes.NewReader([]byte(
		fmt.Sprintf("%d:%v", time.Now().Unix(), err),
	))
}

func okStatus() io.Reader {
	return bytes.NewReader([]byte(
		fmt.Sprintf("%d", time.Now().Unix()),
	))
}

type nodeErr struct {
	node string
	msg  string
}

func (n nodeErr) Error() string {
	return fmt.Sprintf("%s failed: %s", n.node, n.msg)
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	cp := make(map[K]V)
	for k, v := range m {
		cp[k] = v
	}

	return cp
}

func (r *PhysRestore) waitFiles(status pbm.Status, objs map[string]struct{}, cluster bool) (retStatus pbm.Status, err error) {
	if len(objs) == 0 {
		return pbm.StatusError, errors.New("empty objects maps")
	}

	tk := time.NewTicker(time.Second * 5)
	defer tk.Stop()

	retStatus = status

	var curErr error
	var haveDone bool
	for range tk.C {
		for f := range objs {
			errFile := f + "." + string(pbm.StatusError)
			_, err = r.stg.FileStat(errFile)
			if err != nil && err != storage.ErrNotExist {
				return pbm.StatusError, errors.Wrapf(err, "get file %s", errFile)
			}

			if err == nil {
				r, err := r.stg.SourceReader(errFile)
				if err != nil {
					return pbm.StatusError, errors.Wrapf(err, "open error file %s", errFile)
				}

				b, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					return pbm.StatusError, errors.Wrapf(err, "read error file %s", errFile)
				}
				if status != pbm.StatusDone {
					return pbm.StatusError, nodeErr{filepath.Base(f), string(b)}
				}
				curErr = nodeErr{filepath.Base(f), string(b)}
				delete(objs, f)
				continue
			}

			err := r.checkHB(f + "." + syncHbSuffix)
			if err != nil {
				curErr = errors.Wrapf(err, "check heartbeat in %s.%s", f, syncHbSuffix)
				if status != pbm.StatusDone {
					return pbm.StatusError, curErr
				}
				delete(objs, f)
				continue
			}

			ok, err := checkFile(f+"."+string(status), r.stg)
			if err != nil {
				return pbm.StatusError, errors.Wrapf(err, "check file %s", f+"."+string(status))
			}

			if !ok {
				if status != pbm.StatusDone {
					continue
				}

				ok, err := checkFile(f+"."+string(pbm.StatusPartlyDone), r.stg)
				if err != nil {
					return pbm.StatusError, errors.Wrapf(err, "check file %s", f+"."+string(pbm.StatusPartlyDone))
				}

				if !ok {
					continue
				}
				retStatus = pbm.StatusPartlyDone
			}

			haveDone = true
			delete(objs, f)
		}

		if len(objs) == 0 {
			if curErr == nil {
				return retStatus, nil
			}

			if haveDone && !cluster {
				return pbm.StatusPartlyDone, nil
			}

			return pbm.StatusError, curErr
		}
	}

	return pbm.StatusError, storage.ErrNotExist
}

func checkFile(f string, stg storage.Storage) (ok bool, err error) {
	_, err = stg.FileStat(f)

	if err == nil {
		return true, nil
	}

	if err == storage.ErrNotExist || err == storage.ErrEmpty {
		return false, nil
	}

	return false, err
}

type nodeStatus int

const (
	restoreStared nodeStatus = 1 << iota
	restoreDone
)

func (n nodeStatus) is(s nodeStatus) bool { return n&s != 0 }

// log buffer that will dump content to the storage on restore
// finish (whether it's successful or not). It also dumps content
// and reset buffer when logs size hist the limit.
type logBuff struct {
	buf   *bytes.Buffer
	path  string
	cnt   int
	write func(name string, data io.Reader) error
	limit int64
	mx    sync.Mutex
}

func (l *logBuff) Write(p []byte) (n int, err error) {
	l.mx.Lock()
	defer l.mx.Unlock()
	if l.buf.Len()+len(p) > int(l.limit) {
		err := l.flush()
		if err != nil {
			return 0, err
		}
	}

	return l.buf.Write(p)
}

func (l *logBuff) flush() error {
	fname := fmt.Sprintf("%s.%d.log", l.path, l.cnt)
	err := l.write(fname, l.buf)
	if err != nil {
		return errors.Wrapf(err, "write logs buffer to %s", fname)
	}
	l.buf.Reset()
	l.cnt++

	return nil
}

func (l *logBuff) Flush() error {
	l.mx.Lock()
	defer l.mx.Unlock()
	return l.flush()
}

// Snapshot restores data from the physical snapshot.
//
// Initial sync and coordination between nodes happens via `admin.pbmRestore`
// metadata as with logical restores. But later, since mongod being shutdown,
// status sync going via storage (see `PhysRestore.toState`)
//
// Unlike in logical restore, _every_ node of the replicaset is taking part in
// a physical restore. In that way, we avoid logical resync between nodes after
// the restore. Each node in the cluster does:
//   - Stores current replicset config and mongod port.
//   - Checks backup and stops all routine writes to the db.
//   - Stops mongod and wipes out datadir.
//   - Copies backup data to the datadir.
//   - Starts standalone mongod on ephemeral (tmp) port and reset some internal
//     data also setting one-node replicaset and sets oplogTruncateAfterPoint
//     to the backup's `last write`. `oplogTruncateAfterPoint` set the time up
//     to which journals would be replayed.
//   - Starts standalone mongod to recover oplog from journals.
//   - Cleans up data and resets replicaset config to the working state.
//   - Shuts down mongod and agent (the leader also dumps metadata to the storage).
func (r *PhysRestore) Snapshot(cmd *pbm.RestoreCmd, opid pbm.OPID, l *log.Event, stopAgentC chan<- struct{}, pauseHB func()) (err error) {
	l.Debug("port: %d", r.tmpPort)

	meta := &pbm.RestoreMeta{
		Type:     pbm.PhysicalBackup,
		OPID:     opid.String(),
		Name:     cmd.Name,
		Backup:   cmd.BackupName,
		StartTS:  time.Now().Unix(),
		Status:   pbm.StatusInit,
		Replsets: []pbm.RestoreReplset{{Name: r.nodeInfo.Me}},
	}
	if r.nodeInfo.IsClusterLeader() {
		meta.Leader = r.nodeInfo.Me + "/" + r.rsConf.ID
	}

	var progress nodeStatus
	defer func() {
		// set failed status of node on error, but
		// don't mark node as failed after the local restore succeed
		if err != nil && !progress.is(restoreDone) && !errors.Is(err, ErrNoDataForShard) {
			r.MarkFailed(meta, err, !progress.is(restoreStared))
		}

		r.close(err == nil, progress.is(restoreStared) && !progress.is(restoreDone))
	}()

	err = r.init(cmd.Name, opid, l)
	if err != nil {
		return errors.Wrap(err, "init")
	}

	err = r.prepareBackup(cmd.BackupName)
	if err != nil {
		return err
	}
	meta.Type = r.bcp.Type
	err = r.setTmpConf()
	if err != nil {
		return errors.Wrap(err, "set tmp config")
	}

	if meta.Type == pbm.IncrementalBackup {
		meta.BcpChain = make([]string, 0, len(r.files))
		for i := len(r.files) - 1; i >= 0; i-- {
			meta.BcpChain = append(meta.BcpChain, r.files[i].BcpName)
		}
	}

	_, err = r.toState(pbm.StatusStarting)
	if err != nil {
		return errors.Wrap(err, "move to running state")
	}
	l.Debug("%s", pbm.StatusStarting)

	// don't write logs to the mongo anymore
	// but dump it on storage
	r.cn.Logger().SefBuffer(&logBuff{
		buf:   new(bytes.Buffer),
		path:  fmt.Sprintf("%s/%s/rs.%s/log/%s", pbm.PhysRestoresDir, r.name, r.rsConf.ID, r.nodeInfo.Me),
		limit: 1 << 20, // 1Mb
		write: func(name string, data io.Reader) error { return r.stg.Save(name, data, -1) },
	})
	r.cn.Logger().PauseMgo()

	_, err = r.toState(pbm.StatusRunning)
	if err != nil {
		return errors.Wrapf(err, "moving to state %s", pbm.StatusRunning)
	}

	// On this stage, the agent has to be closed on any outcome as mongod
	// is gonna be turned off. Besides, the agent won't be able to listen to
	// the cmd stream anymore and will flood logs with errors on that.
	l.Info("send to stopAgent chan")
	if stopAgentC != nil {
		stopAgentC <- struct{}{}
	}
	// anget will be stopped only after we exit this func
	// so stop heartbeats not to spam logs while the restore is running
	l.Debug("stop agents heartbeats")
	pauseHB()

	l.Info("stopping mongod and flushing old data")
	err = r.flush()
	if err != nil {
		return err
	}

	// A point of no return. From now on, we should clean the dbPath if an
	// error happens before the node is restored successfully.
	// The mongod most probably won't start anyway (or will start in an
	// inconsistent state). But the clean path will allow starting the cluster
	// and doing InitialSync on this node should the restore succeed on other
	// nodes.
	//
	// Should not be set before `r.flush()` as `flush` cleans the dbPath on its
	// own (which sets the no-return point).
	progress |= restoreStared

	l.Info("copying backup data")
	dstat, err := r.copyFiles()
	if err != nil {
		return errors.Wrap(err, "copy files")
	}
	err = r.writeStat(dstat)
	if err != nil {
		r.log.Warning("write download stat: %v", err)
	}

	l.Info("preparing data")
	err = r.prepareData()
	if err != nil {
		return errors.Wrap(err, "prepare data")
	}

	l.Info("recovering oplog as standalone")
	err = r.recoverStandalone()
	if err != nil {
		return errors.Wrap(err, "recover oplog as standalone")
	}

	l.Info("clean-up and reset replicaset config")
	err = r.resetRS()
	if err != nil {
		return errors.Wrap(err, "clean-up, rs_reset")
	}

	l.Info("restore on node succeed")
	// The node at this stage was restored successfully, so we shouldn't
	// clean up dbPath nor write error status for the node whatever happens
	// next.
	progress |= restoreDone

	stat, err := r.toState(pbm.StatusDone)
	if err != nil {
		return errors.Wrapf(err, "moving to state %s", pbm.StatusDone)
	}

	r.log.Info("writing restore meta")
	err = r.dumpMeta(meta, stat, "")
	if err != nil {
		return errors.Wrap(err, "writing restore meta to storage")
	}

	return nil
}

func (r *PhysRestore) writeStat(stat any) error {
	d := struct {
		D any `json:"d"`
	}{
		D: stat,
	}
	b, err := json.Marshal(d)
	if err != nil {
		return errors.Wrap(err, "marshal")
	}

	err = r.stg.Save(r.syncPathNodeStat, bytes.NewBuffer(b), -1)
	if err != nil {
		return errors.Wrap(err, "write")
	}

	return nil
}

func (r *PhysRestore) dumpMeta(meta *pbm.RestoreMeta, s pbm.Status, msg string) error {
	name := fmt.Sprintf("%s/%s.json", pbm.PhysRestoresDir, meta.Name)
	_, err := r.stg.FileStat(name)
	if err == nil {
		r.log.Warning("meta `%s` already exists, trying write %s status with '%s'", name, s, msg)
		return nil
	}
	if err != nil && err != storage.ErrNotExist {
		return errors.Wrapf(err, "check restore meta `%s`", name)
	}

	// We'll try to build as accurate meta as possible but it won't
	// be 100% accurate as not all agents have reported its final state yet
	// The meta generated here is more for debugging porpuses (just in case).
	// `pbm status` and `resync` will always rebuild it from agents' reports
	// not relying solely on this file.
	condsm, err := pbm.GetPhysRestoreMeta(meta.Name, r.stg, r.log)
	if err == nil {
		meta.Replsets = condsm.Replsets
		meta.Status = condsm.Status
		meta.LastTransitionTS = condsm.LastTransitionTS
		meta.Error = condsm.Error
		meta.Hb = condsm.Hb
		meta.Conditions = condsm.Conditions
	}
	if err != nil || s == pbm.StatusError {
		ts := time.Now().Unix()
		meta.Status = s
		meta.Conditions = append(meta.Conditions, &pbm.Condition{Timestamp: ts, Status: s})
		meta.LastTransitionTS = ts
		meta.Error = fmt.Sprintf("%s/%s: %s", r.nodeInfo.SetName, r.nodeInfo.Me, msg)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "\t")
	err = enc.Encode(meta)
	if err != nil {
		return errors.Wrap(err, "encode restore meta")
	}
	err = r.stg.Save(name, &buf, int64(buf.Len()))
	if err != nil {
		return errors.Wrap(err, "write restore meta")
	}

	return nil
}

func (r *PhysRestore) copyFiles() (stat *s3.DownloadStat, err error) {
	readFn := r.stg.SourceReader
	if t, ok := r.stg.(*s3.S3); ok {
		d := t.NewDownload(r.confOpts.NumDownloadWorkers, r.confOpts.MaxDownloadBufferMb, r.confOpts.DownloadChunkMb)
		readFn = d.SourceReader
		defer func() {
			s := d.Stat()
			stat = &s
			r.log.Debug("download stat: %s", s)
		}()
	}

	setName := pbm.MakeReverseRSMapFunc(r.rsMap)(r.nodeInfo.SetName)
	cpbuf := make([]byte, 32*1024)
	for i := len(r.files) - 1; i >= 0; i-- {
		set := r.files[i]
		for _, f := range set.Data {
			src := filepath.Join(set.BcpName, setName, f.Name+set.Cmpr.Suffix())
			if f.Len != 0 {
				src += fmt.Sprintf(".%d-%d", f.Off, f.Len)
			}
			// cut dbpath from destination if there is any (see PBM-1058)
			fname := f.Name
			if set.dbpath != "" {
				fname = strings.TrimPrefix(fname, set.dbpath)
			}
			dst := filepath.Join(r.dbpath, fname)

			err := os.MkdirAll(filepath.Dir(dst), os.ModeDir|0o700)
			if err != nil {
				return stat, errors.Wrapf(err, "create path %s", filepath.Dir(dst))
			}
			// if this is a directory, only ensure it is created.
			if set.BcpName == bcpDir {
				r.log.Info("create dir <%s>", filepath.Dir(f.Name))
				continue
			}

			r.log.Info("copy <%s> to <%s>", src, dst)
			sr, err := readFn(src)
			if err != nil {
				return stat, errors.Wrapf(err, "create source reader for <%s>", src)
			}
			defer sr.Close()

			data, err := compress.Decompress(sr, set.Cmpr)
			if err != nil {
				return stat, errors.Wrapf(err, "decompress object %s", src)
			}
			defer data.Close()

			fw, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, f.Fmode)
			if err != nil {
				return stat, errors.Wrapf(err, "create/open destination file <%s>", dst)
			}
			defer fw.Close()
			if f.Off != 0 {
				_, err := fw.Seek(f.Off, io.SeekStart)
				if err != nil {
					return stat, errors.Wrapf(err, "set file offset <%s>|%d", dst, f.Off)
				}
			}
			_, err = io.CopyBuffer(fw, data, cpbuf)
			if err != nil {
				return stat, errors.Wrapf(err, "copy file <%s>", dst)
			}
			if f.Size != 0 {
				err = fw.Truncate(f.Size)
				if err != nil {
					return stat, errors.Wrapf(err, "truncate file <%s>|%d", dst, f.Size)
				}
			}
		}
	}
	return stat, nil
}

func (r *PhysRestore) prepareData() error {
	err := r.startMongo("--dbpath", r.dbpath,
		"--setParameter", "disableLogicalSessionCacheRefresh=true")
	if err != nil {
		return errors.Wrap(err, "start mongo")
	}

	c, err := tryConn(5, time.Minute*5, r.tmpPort, path.Join(r.dbpath, internalMongodLog))
	if err != nil {
		return errors.Wrap(err, "connect to mongo")
	}

	ctx := context.Background()

	err = c.Database("local").Collection("replset.minvalid").Drop(ctx)
	if err != nil {
		return errors.Wrap(err, "drop replset.minvalid")
	}
	err = c.Database("local").Collection("replset.oplogTruncateAfterPoint").Drop(ctx)
	if err != nil {
		return errors.Wrap(err, "drop replset.oplogTruncateAfterPoint")
	}
	err = c.Database("local").Collection("replset.election").Drop(ctx)
	if err != nil {
		return errors.Wrap(err, "drop replset.election")
	}
	_, err = c.Database("local").Collection("system.replset").DeleteMany(ctx, bson.D{})
	if err != nil {
		return errors.Wrap(err, "delete from system.replset")
	}

	_, err = c.Database("local").Collection("replset.minvalid").InsertOne(ctx,
		bson.M{"_id": primitive.NewObjectID(), "t": -1, "ts": primitive.Timestamp{0, 1}},
	)
	if err != nil {
		return errors.Wrap(err, "insert to replset.minvalid")
	}

	r.log.Debug("oplogTruncateAfterPoint: %v", r.bcp.LastWriteTS)
	_, err = c.Database("local").Collection("replset.oplogTruncateAfterPoint").InsertOne(ctx,
		bson.M{"_id": "oplogTruncateAfterPoint", "oplogTruncateAfterPoint": r.bcp.LastWriteTS},
	)
	if err != nil {
		return errors.Wrap(err, "set oplogTruncateAfterPoint")
	}

	err = shutdown(c, r.dbpath)
	if err != nil {
		return errors.Wrap(err, "shutdown mongo")
	}

	return nil
}

func shutdown(c *mongo.Client, dbpath string) error {
	err := c.Database("admin").RunCommand(context.Background(), bson.D{{"shutdown", 1}}).Err()
	if err != nil && !strings.Contains(err.Error(), "socket was unexpectedly closed") {
		return err
	}

	err = waitMgoShutdown(dbpath)
	if err != nil {
		return errors.Wrap(err, "shutdown")
	}

	return nil
}

func (r *PhysRestore) recoverStandalone() error {
	err := r.startMongo("--dbpath", r.dbpath,
		"--setParameter", "recoverFromOplogAsStandalone=true",
		"--setParameter", "takeUnstableCheckpointOnShutdown=true")
	if err != nil {
		return errors.Wrap(err, "start mongo")
	}

	c, err := tryConn(5, time.Minute*5, r.tmpPort, path.Join(r.dbpath, internalMongodLog))
	if err != nil {
		return errors.Wrap(err, "connect to mongo")
	}

	err = shutdown(c, r.dbpath)
	if err != nil {
		return errors.Wrap(err, "shutdown mongo")
	}

	return nil
}

func (r *PhysRestore) resetRS() error {
	err := r.startMongo("--dbpath", r.dbpath,
		"--setParameter", "disableLogicalSessionCacheRefresh=true",
		"--setParameter", "skipShardingConfigurationChecks=true")
	if err != nil {
		return errors.Wrap(err, "start mongo")
	}

	c, err := tryConn(5, time.Minute*5, r.tmpPort, path.Join(r.dbpath, internalMongodLog))
	if err != nil {
		return errors.Wrap(err, "connect to mongo")
	}

	ctx := context.Background()

	if r.nodeInfo.IsConfigSrv() {
		err = c.Database("config").Collection("mongos").Drop(ctx)
		if err != nil {
			return errors.Wrap(err, "drop config.mongos")
		}
		err = c.Database("config").Collection("lockpings").Drop(ctx)
		if err != nil {
			return errors.Wrap(err, "drop config.lockpings")
		}

		cur, err := c.Database("config").Collection("shards").Find(ctx, bson.D{})
		if err != nil {
			return errors.WithMessage(err, "find: config.shards")
		}

		var docs []struct {
			I string         `bson:"_id"`
			H string         `bson:"host"`
			R map[string]any `bson:",inline"`
		}
		if err := cur.All(ctx, &docs); err != nil {
			return errors.WithMessage(err, "decode: config.shards")
		}

		sMap := r.getShardMapping(r.bcp)
		mapS := pbm.MakeRSMapFunc(sMap)
		ms := []mongo.WriteModel{&mongo.DeleteManyModel{Filter: bson.D{}}}
		for _, doc := range docs {
			doc.I = mapS(doc.I)
			doc.H = r.shards[doc.I]
			ms = append(ms, &mongo.InsertOneModel{Document: doc})
		}

		_, err = c.Database("config").Collection("shards").BulkWrite(ctx, ms)
		if err != nil {
			return errors.Wrap(err, "update config.shards")
		}

		if len(sMap) != 0 {
			r.log.Debug("updating router config")
			if err := updateRouterTables(ctx, c, sMap); err != nil {
				return errors.WithMessage(err, "update router tables")
			}
		}
	} else {
		var currS string
		for s, uri := range r.shards {
			rs, _, _ := strings.Cut(uri, "/")
			if rs == r.nodeInfo.SetName {
				currS = s
				break
			}
		}

		_, err = c.Database("admin").Collection("system.version").UpdateOne(
			ctx,
			bson.D{{"_id", "shardIdentity"}},
			bson.D{{"$set", bson.M{
				"shardName":                 currS,
				"configsvrConnectionString": r.cfgConn,
			}}},
		)
		if err != nil {
			return errors.Wrap(err, "update shardIdentity in admin.system.version")
		}
	}

	colls, err := c.Database("config").ListCollectionNames(ctx, bson.D{{"name", bson.M{"$regex": `^cache\.`}}})
	if err != nil {
		return errors.WithMessage(err, "list cache collections")
	}
	for _, coll := range colls {
		if err := c.Database("config").Collection(coll).Drop(ctx); err != nil {
			return errors.Wrapf(err, "drop %q", coll)
		}
	}

	const retry = 5
	for i := 0; i < retry; i++ {
		err = c.Database("config").Collection("system.sessions").Drop(ctx)
		if err == nil || !strings.Contains(err.Error(), "(BackgroundOperationInProgressForNamespace)") {
			break
		}
		r.log.Debug("drop config.system.sessions: BackgroundOperationInProgressForNamespace, retrying")
		time.Sleep(time.Second * time.Duration(i+1))
	}
	if err != nil {
		return errors.Wrap(err, "drop config.system.sessions")
	}

	_, err = c.Database("local").Collection("system.replset").DeleteMany(ctx, bson.D{})
	if err != nil {
		return errors.Wrap(err, "delete from system.replset")
	}

	_, err = c.Database("local").Collection("system.replset").InsertOne(ctx,
		pbm.RSConfig{
			ID:       r.rsConf.ID,
			CSRS:     r.nodeInfo.IsConfigSrv(),
			Version:  1,
			Members:  r.rsConf.Members,
			Settings: r.rsConf.Settings,
		},
	)
	if err != nil {
		return errors.Wrapf(err, "update rs.member host to %s", r.nodeInfo.Me)
	}

	// PITR should be turned off after the physical restore. Otherwise, slicing resumes
	// right after the cluster start while the system in the state of the backup's
	// recovery time. No resync yet. Hence the system knows nothing about the recent
	// restore and chunks made after the backup. So it would successfully start slicing
	// and overwrites chunks after the backup.
	if r.nodeInfo.IsLeader() {
		_, err = c.Database(pbm.DB).Collection(pbm.ConfigCollection).UpdateOne(ctx, bson.D{},
			bson.D{{"$set", bson.M{"pitr.enabled": false}}},
		)
		if err != nil {
			return errors.Wrap(err, "turn off pitr")
		}
	}

	err = shutdown(c, r.dbpath)
	if err != nil {
		return errors.Wrap(err, "shutdown mongo")
	}

	return nil
}

func (r *PhysRestore) getShardMapping(bcp *pbm.BackupMeta) map[string]string {
	source := make(map[string]string)
	if bcp.ShardRemap != nil {
		for i := range bcp.Replsets {
			rs := bcp.Replsets[i].Name
			if s, ok := bcp.ShardRemap[rs]; ok {
				source[rs] = s
			}
		}
	}

	mapRevRS := pbm.MakeReverseRSMapFunc(r.rsMap)
	rv := make(map[string]string)
	for targetS, uri := range r.shards {
		targetRS, _, _ := strings.Cut(uri, "/")
		sourceRS := mapRevRS(targetRS)
		sourceS, ok := source[sourceRS]
		if ok && sourceS != targetS {
			rv[sourceS] = targetS
		}
	}

	return rv
}

// Tries to connect to mongo n times, timeout is applied for each try.
// If a try is unsuccessful, it will check the mongo logs and retry if
// there are no errors or fatals.
func tryConn(n int, tout time.Duration, port int, logpath string) (cn *mongo.Client, err error) {
	type mlog struct {
		T struct {
			Date string `json:"$date"`
		} `json:"t"`
		S   string `json:"s"`
		Msg string `json:"msg"`
	}
	for i := 0; i < n; i++ {
		cn, err = conn(port, tout)
		if err == nil {
			return cn, nil
		}

		f, ferr := os.Open(logpath)
		if ferr != nil {
			return nil, errors.Errorf("open logs: %v, connect err: %v", ferr, err)
		}
		defer f.Close()

		dec := json.NewDecoder(f)
		for {
			var m mlog
			if derr := dec.Decode(&m); derr == io.EOF {
				break
			} else if derr != nil {
				return nil, errors.Errorf("decode logs: %v, connect err: %v", derr, err)
			}
			if m.S == "E" || m.S == "F" {
				return nil, errors.Errorf("mongo failed with [%s] %s / %s, connect err: %v", m.S, m.Msg, m.T.Date, err)
			}
		}
	}

	return nil, errors.Errorf("failed to  connect after %d tries: %v", n, err)
}

func conn(port int, tout time.Duration) (*mongo.Client, error) {
	ctx := context.Background()

	opts := options.Client().
		SetHosts([]string{"localhost:" + strconv.Itoa(port)}).
		SetAppName("pbm-physical-restore").
		SetDirect(true).
		SetConnectTimeout(time.Second * 120).
		SetServerSelectionTimeout(tout)

	conn, err := mongo.NewClient(opts)
	if err != nil {
		return nil, errors.Wrap(err, "create mongo client")
	}

	err = conn.Connect(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "connect")
	}

	err = conn.Ping(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "ping")
	}

	return conn, nil
}

const internalMongodLog = "pbm.restore.log"

func (r *PhysRestore) startMongo(opts ...string) error {
	if r.tmpConf != nil {
		opts = append(opts, []string{"-f", r.tmpConf.Name()}...)
	}

	opts = append(opts, []string{"--logpath", path.Join(r.dbpath, internalMongodLog)}...)

	errBuf := new(bytes.Buffer)
	cmd := exec.Command(r.mongod, opts...)

	cmd.Stderr = errBuf
	err := cmd.Start()
	if err != nil {
		return err
	}

	// release process resources
	go func() {
		err := cmd.Wait()
		if err != nil {
			slog.Printf("mongod process: %v, %s", err, errBuf)
		}
	}()
	return nil
}

const hbFrameSec = 60 * 2

func (r *PhysRestore) init(name string, opid pbm.OPID, l *log.Event) (err error) {
	var cfg pbm.Config
	cfg, err = r.cn.GetConfig()
	if err != nil {
		return errors.Wrap(err, "get pbm config")
	}

	r.stg, err = pbm.Storage(cfg, l)
	if err != nil {
		return errors.Wrap(err, "get storage")
	}

	r.confOpts = cfg.Restore

	r.mongod = "mongod" // run from $PATH by default
	if r.confOpts.MongodLocation != "" {
		r.mongod = r.confOpts.MongodLocation
	}
	if m, ok := r.confOpts.MongodLocationMap[r.nodeInfo.Me]; ok {
		r.mongod = m
	}

	r.log = l

	r.name = name
	r.opid = opid.String()

	r.startTS = time.Now().Unix()

	r.syncPathNode = fmt.Sprintf("%s/%s/rs.%s/node.%s", pbm.PhysRestoresDir, r.name, r.rsConf.ID, r.nodeInfo.Me)
	r.syncPathNodeStat = fmt.Sprintf("%s/%s/rs.%s/stat.%s", pbm.PhysRestoresDir, r.name, r.rsConf.ID, r.nodeInfo.Me)
	r.syncPathRS = fmt.Sprintf("%s/%s/rs.%s/rs", pbm.PhysRestoresDir, r.name, r.rsConf.ID)
	r.syncPathCluster = fmt.Sprintf("%s/%s/cluster", pbm.PhysRestoresDir, r.name)
	r.syncPathPeers = make(map[string]struct{})
	for _, m := range r.rsConf.Members {
		if !m.ArbiterOnly {
			r.syncPathPeers[fmt.Sprintf("%s/%s/rs.%s/node.%s", pbm.PhysRestoresDir, r.name, r.rsConf.ID, m.Host)] = struct{}{}
		}
	}
	if r.nodeInfo.IsConfigSrv() {
		sh, err := r.cn.GetShards()
		if err != nil {
			return errors.Wrap(err, "get data shards")
		}
		r.syncPathDataShards = make(map[string]struct{})
		for _, s := range sh {
			r.syncPathDataShards[fmt.Sprintf("%s/%s/rs.%s/rs", pbm.PhysRestoresDir, r.name, s.RS)] = struct{}{}
		}
	}

	err = r.hb()
	if err != nil {
		l.Error("send init heartbeat: %v", err)
	}

	r.stopHB = make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Second * hbFrameSec)
		defer func() {
			tk.Stop()
			l.Debug("hearbeats stopped")
		}()
		for {
			select {
			case <-tk.C:
				err := r.hb()
				if err != nil {
					l.Warning("send heartbeat: %v", err)
				}
			case <-r.stopHB:
				return
			}
		}
	}()

	return nil
}

const syncHbSuffix = "hb"

func (r *PhysRestore) hb() error {
	ts := time.Now().Unix()

	err := r.stg.Save(r.syncPathNode+"."+syncHbSuffix,
		bytes.NewReader([]byte(strconv.FormatInt(ts, 10))), -1)
	if err != nil {
		return errors.Wrap(err, "write node hb")
	}

	err = r.stg.Save(r.syncPathRS+"."+syncHbSuffix,
		bytes.NewReader([]byte(strconv.FormatInt(ts, 10))), -1)
	if err != nil {
		return errors.Wrap(err, "write rs hb")
	}

	err = r.stg.Save(r.syncPathCluster+"."+syncHbSuffix,
		bytes.NewReader([]byte(strconv.FormatInt(ts, 10))), -1)
	if err != nil {
		return errors.Wrap(err, "write rs hb")
	}

	return nil
}

func (r *PhysRestore) checkHB(file string) error {
	ts := time.Now().Unix()

	_, err := r.stg.FileStat(file)
	// compare with restore start if heartbeat files are yet to be created.
	// basically wait another hbFrameSec*2 sec for heartbeat files.
	if errors.Is(err, storage.ErrNotExist) {
		if r.startTS+hbFrameSec*2 < ts {
			return errors.Errorf("stuck, last beat ts: %d", r.startTS)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "get file stat")
	}

	f, err := r.stg.SourceReader(file)
	if err != nil {
		return errors.Wrap(err, "get hb file")
	}

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return errors.Wrap(err, "read content")
	}

	t, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 0)
	if err != nil {
		return errors.Wrap(err, "decode")
	}

	if t+hbFrameSec*2 < ts {
		return errors.Errorf("stuck, last beat ts: %d", t)
	}

	return nil
}

func (r *PhysRestore) setTmpConf() (err error) {
	setName := pbm.MakeReverseRSMapFunc(r.rsMap)(r.nodeInfo.SetName)
	opts := new(pbm.MongodOpts)
	for _, v := range r.bcp.Replsets {
		if v.Name == setName {
			if v.MongodOpts == nil {
				return nil
			}

			opts.Storage = v.MongodOpts.Storage
			break
		}
	}

	opts.Net.BindIp = "localhost"
	opts.Net.Port = r.tmpPort
	opts.Storage.DBpath = r.dbpath
	opts.Security = r.secOpts

	r.tmpConf, err = os.CreateTemp("", "pbmMongdTmpConf")
	if err != nil {
		return errors.Wrap(err, "create tmp config")
	}
	defer r.tmpConf.Close()

	enc := yaml.NewEncoder(r.tmpConf)
	err = enc.Encode(opts)
	if err != nil {
		return errors.Wrap(err, "encode options")
	}
	err = enc.Close()
	if err != nil {
		return errors.Wrap(err, "close encoder")
	}
	err = r.tmpConf.Sync()
	if err != nil {
		return errors.Wrap(err, "fsync")
	}

	return nil
}

const bcpDir = "__dir__"

// Sets replset files that have to be copied to the target during the restore.
// For non-incremental backups it's just the content of backups files (data) and
// journals. For the incrementals, it will gather files from preceding backups
// travelling back in time from the target backup up to the closest base.
// `Off == -1 && Len == -1` means the file remains unchanged since the last
// backup and there is no data in this backup. We need such info in
// the target backup to know which files from preceding backups should be
// restored. Only files listed in the target backup will be restored.
//
// The restore should be done in reverse order. Applying files (diffs)
// starting from the base and moving forward in time up to the target backup.
func (r *PhysRestore) setBcpFiles() (err error) {
	bcp := r.bcp

	setName := pbm.MakeReverseRSMapFunc(r.rsMap)(r.nodeInfo.SetName)
	rs := getRS(bcp, setName)
	if rs == nil {
		return errors.Errorf("no data in the backup for the replica set %s", setName)
	}

	targetFiles := make(map[string]bool)
	for _, f := range append(rs.Files, rs.Journal...) {
		targetFiles[f.Name] = false
	}

	for {
		data := files{
			BcpName: bcp.Name,
			Cmpr:    bcp.Compression,
			Data:    []pbm.File{},
		}
		// PBM-1058
		var is1058 bool
		for _, f := range append(rs.Files, rs.Journal...) {
			if _, ok := targetFiles[f.Name]; ok && f.Off >= 0 && f.Len >= 0 {
				data.Data = append(data.Data, f)
				targetFiles[f.Name] = true

				if data.dbpath == "" {
					is1058, data.dbpath = findDBpath(f.Name)
				}
			}
		}
		if is1058 {
			r.log.Debug("issue PBM-1058 detected in backup %s, dbpath is %s", bcp.Name, data.dbpath)
		}

		r.files = append(r.files, data)

		if bcp.SrcBackup == "" {
			break
		}

		r.log.Debug("get src %s", bcp.SrcBackup)
		bcp, err = r.cn.GetBackupMeta(bcp.SrcBackup)
		if err != nil {
			return errors.Wrapf(err, "get source backup")
		}
		rs = getRS(bcp, setName)
	}

	// Directories only. Incremental $backupCusor returns collections that
	// were created but haven't ended up in the checkpoint yet in the format
	// if they don't belong to this backup (see PBM-1063). PBM doesn't copy
	// such files. But they are already in WT metadata. PSMDB (WT) handles
	// this by creating such files during the start. But fails to do so if
	// it runs with `directoryPerDB` option. Namely fails to create a directory
	// for the collections. So we have to detect and create these directories
	// during the restore.
	var dirs []pbm.File
	dirsm := make(map[string]struct{})
	for f, was := range targetFiles {
		if !was {
			dir := path.Dir(f)
			if _, ok := dirsm[dir]; dir != "." && !ok {
				dirs = append(dirs, pbm.File{
					Name: f,
					Off:  -1,
					Len:  -1,
					Size: -1,
				})
				dirsm[dir] = struct{}{}
			}
		}
	}

	if len(dirs) > 0 {
		r.files = append(r.files, files{
			BcpName: bcpDir,
			Data:    dirs,
		})
	}

	return nil
}

// Checks if dbpath exists in the file name (affected by PBM-1058) and
// returns it.
// We suppose that "journal" will always be present in the backup and it is
// always in the dbpath root and doesn't contain subdirs, only files. Having
// a leading `/` indicates that restore was affected by PBM-1058 but only
// with journal files we may detect the exact prefix.
func findDBpath(fname string) (is bool, prefix string) {
	if !strings.HasPrefix(fname, "/") {
		return false, ""
	}

	is = true
	d, _ := path.Split(fname)
	if strings.HasSuffix(d, "/journal/") {
		prefix = path.Dir(d[0 : len(d)-1])
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return is, prefix
}

func getRS(bcp *pbm.BackupMeta, rs string) *pbm.BackupReplset {
	for _, r := range bcp.Replsets {
		if r.Name == rs {
			return &r
		}
	}
	return nil
}

func (r *PhysRestore) prepareBackup(backupName string) (err error) {
	r.bcp, err = r.cn.GetBackupMeta(backupName)
	if errors.Is(err, pbm.ErrNotFound) {
		r.bcp, err = GetMetaFromStore(r.stg, backupName)
	}
	if err != nil {
		return errors.Wrap(err, "get backup metadata")
	}

	if r.bcp == nil {
		return errors.New("snapshot name doesn't set")
	}

	err = r.cn.SetRestoreBackup(r.name, r.bcp.Name, nil)
	if err != nil {
		return errors.Wrap(err, "set backup name")
	}

	if r.bcp.Status != pbm.StatusDone {
		return errors.Errorf("backup wasn't successful: status: %s, error: %s", r.bcp.Status, r.bcp.Error())
	}

	if !version.CompatibleWith(r.bcp.PBMVersion, pbm.BreakingChangesMap[r.bcp.Type]) {
		return errors.Errorf("backup version (v%s) is not compatible with PBM v%s", r.bcp.PBMVersion, version.DefaultInfo.Version)
	}

	mgoV, err := r.node.GetMongoVersion()
	if err != nil || len(mgoV.Version) < 1 {
		return errors.Wrap(err, "define mongo version")
	}

	if semver.Compare(majmin(r.bcp.MongoVersion), majmin(mgoV.VersionString)) != 0 {
		return errors.Errorf("backup's Mongo version (%s) is not compatible with Mongo %s", r.bcp.MongoVersion, mgoV.VersionString)
	}

	mv, err := r.checkMongod(r.bcp.MongoVersion)
	if err != nil {
		return errors.Wrap(err, "check mongod binary")
	}
	r.log.Debug("mongod binary: %s, version: %s", r.mongod, mv)

	err = r.setBcpFiles()
	if err != nil {
		return errors.Wrap(err, "get data for restore")
	}

	s, err := r.cn.ClusterMembers()
	if err != nil {
		return errors.Wrap(err, "get cluster members")
	}

	mapRevRS := pbm.MakeReverseRSMapFunc(r.rsMap)
	fl := make(map[string]pbm.Shard, len(s))
	r.syncPathShards = make(map[string]struct{})
	for _, rs := range s {
		fl[mapRevRS(rs.RS)] = rs
		r.syncPathShards[fmt.Sprintf("%s/%s/rs.%s/rs", pbm.PhysRestoresDir, r.name, rs.RS)] = struct{}{}
	}

	var nors []string
	for _, sh := range r.bcp.Replsets {
		if _, ok := fl[sh.Name]; !ok {
			nors = append(nors, sh.Name)
		}
	}

	if len(nors) > 0 {
		return errors.Errorf("extra/unknown replica set found in the backup: %s", strings.Join(nors, ","))
	}

	setName := mapRevRS(r.nodeInfo.SetName)
	var ok bool
	for _, v := range r.bcp.Replsets {
		if v.Name == setName {
			ok = true
			break
		}
	}
	if !ok {
		if r.nodeInfo.IsLeader() {
			return errors.New("no data for the config server or sole rs in backup")
		}
		return ErrNoDataForShard
	}

	return nil
}

// ensure mongod for internal restarts is available and matches
// the backup's version
func (r *PhysRestore) checkMongod(needVersion string) (version string, err error) {
	cmd := exec.Command(r.mongod, "--version")

	stderr := new(bytes.Buffer)
	stdout := new(bytes.Buffer)

	cmd.Stderr = stderr
	cmd.Stdout = stdout

	err = cmd.Run()
	if err != nil {
		return "", errors.Errorf("run: %v. stderr: %s", err, stderr)
	}

	_, v, ok := strings.Cut(strings.Split(stdout.String(), "\n")[0], "db version ")
	if !ok {
		return "", errors.Errorf("parse version from output %s", stdout.String())
	}

	if semver.Compare(majmin(needVersion), majmin(v)) != 0 {
		return "", errors.Errorf("backup's Mongo version (%s) is not compatible with mongod %s", needVersion, v)
	}

	return v, nil
}

// MarkFailed sets the restore and rs state as failed with the given message
func (r *PhysRestore) MarkFailed(meta *pbm.RestoreMeta, e error, markCluster bool) {
	var nerr nodeErr
	if errors.As(e, &nerr) {
		e = nerr
		meta.Replsets = []pbm.RestoreReplset{{
			Name:   nerr.node,
			Status: pbm.StatusError,
			Error:  nerr.msg,
		}}
	} else if len(meta.Replsets) > 0 {
		meta.Replsets[0].Status = pbm.StatusError
		meta.Replsets[0].Error = e.Error()
	}

	err := r.stg.Save(r.syncPathNode+"."+string(pbm.StatusError),
		errStatus(e), -1)
	if err != nil {
		r.log.Error("write error state `%v` to storage: %v", e, err)
	}

	// At some point, every node will try to set an rs and cluster state
	// (in `toState` method).
	// Here we are not aware of partlyDone etc so leave it to the `toState`.
	if r.nodeInfo.IsPrimary && markCluster {
		serr := r.stg.Save(r.syncPathRS+"."+string(pbm.StatusError),
			errStatus(e), -1)
		if serr != nil {
			r.log.Error("MarkFailed: write replset error state `%v`: %v", e, serr)
		}
	}
	if r.nodeInfo.IsClusterLeader() && markCluster {
		serr := r.stg.Save(r.syncPathCluster+"."+string(pbm.StatusError),
			errStatus(e), -1)
		if serr != nil {
			r.log.Error("MarkFailed: write cluster error state `%v`: %v", e, serr)
		}
	}
}

func removeAll(dir string, l *log.Event) error {
	d, err := os.Open(dir)
	if err != nil {
		return errors.Wrap(err, "open dir")
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return errors.Wrap(err, "read file names")
	}
	for _, n := range names {
		if n == internalMongodLog {
			continue
		}
		err = os.RemoveAll(filepath.Join(dir, n))
		if err != nil {
			return errors.Wrapf(err, "remove '%s'", n)
		}
		l.Debug("remove %s", filepath.Join(dir, n))
	}
	return nil
}

func majmin(v string) string {
	if len(v) == 0 {
		return v
	}

	if v[0] != 'v' {
		v = "v" + v
	}

	return semver.MajorMinor(v)
}
