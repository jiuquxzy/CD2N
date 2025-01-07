package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/CD2N/CD2N/retriever/config"
	"github.com/CD2N/CD2N/retriever/libs/buffer"
	"github.com/CD2N/CD2N/retriever/libs/chain"
	"github.com/CD2N/CD2N/retriever/libs/client"
	"github.com/CD2N/CD2N/retriever/libs/task"
	"github.com/CD2N/CD2N/retriever/logger"
	"github.com/CD2N/CD2N/retriever/utils"
	cess "github.com/CESSProject/cess-go-sdk/chain"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
)

type FileRequest struct {
	Pubkey    []byte `json:"pubkey"`
	Fid       string `json:"fid"`
	Timestamp string `json:"timestamp"`
	Sign      string `json:"sign"`
}

type FileResponse struct {
	Fid       string   `json:"fid"`
	Fragments []string `json:"fragments"`
	Token     string   `json:"token"`
}

func (g *Gateway) ProvideFile(ctx context.Context, exp time.Duration, info task.FileInfo) error {
	if _, ok := g.pstats.Fids.LoadOrStore(info.Fid, struct{}{}); ok {
		return errors.Wrap(errors.New("file is being processed"), "provide file error")
	}

	rand, _ := utils.GetRandomBytes()
	conf := config.GetConfig()
	ftask := task.Task{
		Tid:       hex.EncodeToString(rand[:task.TID_BYTES_LEN]),
		Exp:       int64(exp),
		Acc:       g.contract.Node.Hex(),
		Addr:      conf.Endpoint,
		Did:       info.Fid,
		Timestamp: time.Now().Format(config.TIME_LAYOUT),
	}
	provideTask := task.ProvideTask{
		Task:      ftask,
		FileInfo:  info,
		GroupSize: len(info.Fragments),
		SubTasks:  make(map[string]task.ProvideSubTask),
	}
	hash, err := g.CreateStorageOrder(info)
	defer func() {
		if err != nil {
			g.pstats.Fids.Delete(info.Fid)
		}
	}()
	if err != nil {

		return errors.Wrap(err, "provide file error")
	} else {
		logger.GetLogger(config.LOG_PROVIDER).Infof("create storage order for file %s, tx hash is %s \n", info.Fid, hash)
	}

	err = client.PutData(g.taskRecord, info.Fid, provideTask)
	if err != nil {
		return errors.Wrap(err, "provide file error")
	}
	err = client.PublishMessage(g.redisCli, ctx, client.CHANNEL_PROVIDE, ftask)
	if err != nil {
		return errors.Wrap(err, "provide file error")
	}
	g.pstats.Ongoing.Add(1)
	return nil
}

func (g *Gateway) ClaimFile(ctx context.Context, req FileRequest) (FileResponse, error) {
	var resp FileResponse
	sign, err := hex.DecodeString(req.Sign)
	if err != nil {
		return resp, errors.Wrap(err, "claim file error")
	}
	date, err := time.Parse(config.TIME_LAYOUT, req.Timestamp)
	if err != nil {
		return resp, errors.Wrap(err, "claim file error")
	}
	if time.Since(date) > time.Second*15 {
		return resp, errors.Wrap(errors.New("expired request"), "claim file error")
	}
	req.Sign = ""
	jbytes, err := json.Marshal(req)
	if err != nil {
		return resp, errors.Wrap(err, "claim file error")
	}
	if !utils.VerifySecp256k1Sign(req.Pubkey, jbytes, sign) {
		return resp, errors.Wrap(errors.New("signature verification failed"), "claim file error")
	}

	if _, ok := g.pstats.Fids.Load(req.Fid); !ok {
		return resp, errors.Wrap(errors.New("the file has been distributed"), "claim file error")
	}

	g.keyLock.Lock(req.Fid)
	var ftask task.ProvideTask
	err = client.GetData(g.taskRecord, req.Fid, &ftask)
	if err != nil {
		g.keyLock.Delete(req.Fid) // task done, remove the key-value lock
		return resp, errors.Wrap(err, "claim file error")
	}
	defer g.keyLock.Unlock(req.Fid)
	if len(ftask.SubTasks) == task.PROVIDE_TASK_GROUP_NUM {
		return resp, errors.Wrap(errors.New("file be claimed"), "claim file error")
	}
	gid := ftask.AddSubTask()
	if gid == -1 {
		return resp, errors.Wrap(errors.New("all subtasks have been distributed"), "claim file error")
	}

	key, err := crypto.DecompressPubkey(req.Pubkey)
	if err != nil {
		return resp, errors.Wrap(err, "claim file error")
	}
	for {
		token, _ := utils.GetRandomBytes()
		resp.Token = hex.EncodeToString(token[:task.TID_BYTES_LEN])
		if _, ok := ftask.SubTasks[resp.Token]; !ok {
			break
		}
	}
	ftask.SubTasks[resp.Token] = task.ProvideSubTask{
		Claimant:  crypto.PubkeyToAddress(*key).Hex(),
		GroupId:   gid,
		Timestamp: time.Now().Format(config.TIME_LAYOUT),
	}
	resp.Fragments = make([]string, 0, ftask.GroupSize)
	for i := 0; i < ftask.GroupSize; i++ {
		resp.Fragments = append(resp.Fragments, ftask.Fragments[i][gid])
	}
	resp.Fid = req.Fid
	err = client.PutData(g.taskRecord, req.Fid, ftask)
	if err != nil {
		return resp, errors.Wrap(err, "claim file error")
	}
	return resp, nil
}

func (g *Gateway) FetchFile(ctx context.Context, fid, did, token string) (string, error) {
	var fpath string
	if _, ok := g.pstats.Fids.LoadOrStore(fid, struct{}{}); !ok {
		return fpath, errors.Wrap(errors.New("wrong file id"), "fetch file error")
	}
	g.keyLock.Lock(fid)
	defer g.keyLock.Unlock(fid)
	var task task.ProvideTask
	err := client.GetData(g.taskRecord, fid, &task)
	if err != nil {
		return fpath, errors.Wrap(err, "fetch file error")
	}
	subTask, ok := task.SubTasks[token]
	if !ok {
		return fpath, errors.Wrap(errors.New("subtask not found"), "fetch file error")
	}
	if subTask.Index == task.GroupSize {
		return fpath, errors.Wrap(errors.New("subtask done"), "fetch file error")
	}
	fpath = filepath.Join(task.BaseDir, task.Fragments[subTask.Index][subTask.GroupId])
	subTask.Index++
	task.SubTasks[token] = subTask
	err = client.PutData(g.taskRecord, fid, task)
	if err != nil {
		return fpath, errors.Wrap(err, "fetch file error")
	}
	return fpath, nil
}

func (g *Gateway) ProvideTaskChecker(ctx context.Context, buffer *buffer.FileBuffer) error {
	ticker := time.NewTicker(task.PROVIDE_TASK_CHECK_TIME)
	for {
		select {
		case <-ticker.C:
			if err := g.checker(ctx, buffer); err != nil {
				logger.GetLogger(config.LOG_PROVIDER).Error(err)
			}
		case <-ctx.Done():
			return errors.New("provide task checker done.")
		}
	}
}

func (g *Gateway) checker(ctx context.Context, buffer *buffer.FileBuffer) error {
	var ftask task.ProvideTask
	err := client.DbIterator(g.taskRecord,
		func(key []byte) error {
			select {
			case <-ctx.Done():
			default:
			}
			fid := string(key)
			g.keyLock.Lock(fid)
			defer g.keyLock.Unlock(fid)
			if err := client.GetData(g.taskRecord, fid, &ftask); err != nil {
				return err
			}
			if ftask.WorkDone {
				g.pstats.TaskDone(fid)
				g.keyLock.RemoveLock(fid)
				logger.GetLogger(config.LOG_PROVIDER).Infof("file %s distribute workflow done. \n", fid)
				return client.DeleteData(g.taskRecord, fid)
			}
			done := 0
			cli, err := g.GetCessClient()
			if err != nil {
				logger.GetLogger(config.LOG_PROVIDER).Error(err)
				return nil
			}
			cmpSet, err := chain.QueryDealMap(cli, fid)
			if err == nil {
				for k, v := range ftask.SubTasks {
					if _, ok := cmpSet[v.GroupId+1]; v.Index == ftask.GroupSize && ok {
						v.Done = time.Now().Format(config.TIME_LAYOUT)
						done++
						ftask.SubTasks[k] = v
						if err = RemoveSubTaskFiles(buffer, v.GroupId, ftask); err != nil {
							logger.GetLogger(config.LOG_PROVIDER).Error(err)
						}
						continue
					}
					upt, err := time.Parse(config.TIME_LAYOUT, v.Timestamp)
					if err != nil {
						continue
					}
					if time.Since(upt) >= task.PROVIDE_TASK_CHECK_TIME*2 {
						logger.GetLogger(config.LOG_PROVIDER).Infof("remove subtask %d of file %s, timeout!", v.GroupId+1, fid)
						ftask.DelSubTask(v.GroupId)
						delete(ftask.SubTasks, k)
					}
				}
			} else if strings.Contains(err.Error(), "empty") {
				logger.GetLogger(config.LOG_PROVIDER).Infof("file %s deal map empty %v", fid, err)
				done = task.PROVIDE_TASK_GROUP_NUM
			} else {
				logger.GetLogger(config.LOG_PROVIDER).Error(err)
				return nil
			}

			if done == task.PROVIDE_TASK_GROUP_NUM {
				logger.GetLogger(config.LOG_PROVIDER).Infof("file %s be distributed. \n", fid)
				ftask.WorkDone = true
			} else if len(ftask.SubTasks) < task.PROVIDE_TASK_GROUP_NUM {
				err := client.PublishMessage(g.redisCli, ctx, client.CHANNEL_PROVIDE, ftask.Task)
				if err != nil {
					return err
				}
				ftask.Retry += 1
				g.pstats.TaskFlash(fid)
			}
			if err := client.PutData(g.taskRecord, fid, ftask); err != nil {
				return err
			}
			if done == task.PROVIDE_TASK_GROUP_NUM {
				//remove fid from provide task stats
				g.pstats.Fids.Delete(fid)
			}
			return nil
		},
	)
	return errors.Wrap(err, "check provide task error")
}

func RemoveSubTaskFiles(buffer *buffer.FileBuffer, groupId int, ftask task.ProvideTask) error {
	for i := 0; i < len(ftask.Fragments); i++ {
		err := buffer.RemoveData(filepath.Join(ftask.BaseDir, ftask.Fragments[i][groupId]))
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) CreateStorageOrder(info task.FileInfo) (string, error) {
	var segments []cess.SegmentDataInfo
	for i, v := range info.Fragments {
		segments = append(segments, cess.SegmentDataInfo{
			SegmentHash:  info.Segments[i],
			FragmentHash: v,
		})
	}
	cli, err := g.GetCessClient()
	if err != nil {
		return "", errors.Wrap(err, "create storage order error")
	}
	hash, err := chain.CreateStorageOrder(
		cli, info.Fid, info.FileName,
		info.Territory, segments, info.Owner, uint64(info.FileSize),
	)
	if err != nil {
		return "", errors.Wrap(err, "create storage order error")
	}
	return hash, nil
}

func (g *Gateway) GetCessClient() (cess.Chainer, error) {
	if g.cessCli != nil {
		if _, err := g.cessCli.QueryBlockNumber(""); err == nil {
			return g.cessCli, nil
		}
	}
	conf := config.GetConfig()
	cli, err := chain.NewCessChainClient(context.Background(), conf.Mnemonic, conf.Rpcs)
	if err != nil {
		return nil, errors.Wrap(err, "get cess client error")
	}
	g.cessCli = cli
	return cli, nil
}