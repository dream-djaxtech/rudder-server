package warehouse

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-server/warehouse/warehousemanager"

	"github.com/bugsnag/bugsnag-go"
	"github.com/lib/pq"
	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/db"
	destinationConnectionTester "github.com/rudderlabs/rudder-server/services/destination-connection-tester"
	"github.com/rudderlabs/rudder-server/services/pgnotifier"
	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/services/validators"
	"github.com/rudderlabs/rudder-server/utils"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/timeutil"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
	"github.com/tidwall/gjson"
)

var (
	webPort                          int
	dbHandle                         *sql.DB
	notifier                         pgnotifier.PgNotifierT
	WarehouseDestinations            []string
	jobQueryBatchSize                int
	noOfWorkers                      int
	noOfSlaveWorkerRoutines          int
	slaveWorkerRoutineBusy           []bool //Busy-true
	uploadFreqInS                    int64
	stagingFilesSchemaPaginationSize int
	mainLoopSleep                    time.Duration
	workerRetrySleep                 time.Duration
	stagingFilesBatchSize            int
	configSubscriberLock             sync.RWMutex
	crashRecoverWarehouses           []string
	inProgressMap                    map[string]bool
	inRecoveryMap                    map[string]bool
	inProgressMapLock                sync.RWMutex
	lastExecMap                      map[string]int64
	lastExecMapLock                  sync.RWMutex
	warehouseMode                    string
	warehouseSyncPreFetchCount       int
	warehouseSyncFreqIgnore          bool
	preLoadedIdentitiesMap           map[string]bool
	preLoadedIdentitiesMapLock       sync.RWMutex
	activeWorkerCount                int
	activeWorkerCountLock            sync.RWMutex
)

var (
	host, user, password, dbname, sslmode string
	port                                  int
)

// warehouses worker modes
const (
	MasterMode      = "master"
	SlaveMode       = "slave"
	MasterSlaveMode = "master_and_slave"
	EmbeddedMode    = "embedded"
)

type HandleT struct {
	destType             string
	warehouses           []warehouseutils.WarehouseT
	dbHandle             *sql.DB
	notifier             pgnotifier.PgNotifierT
	uploadToWarehouseQ   chan []ProcessStagingFilesJobT
	createLoadFilesQ     chan LoadFileJobT
	isEnabled            bool
	configSubscriberLock sync.RWMutex
	workerChannelMap     map[string]chan []ProcessStagingFilesJobT
	workerChannelMapLock sync.RWMutex
}

type ErrorResponseT struct {
	Error string
}

func init() {
	loadConfig()
}

func loadConfig() {
	//Port where WH is running
	webPort = config.GetInt("Warehouse.webPort", 8082)
	WarehouseDestinations = []string{"RS", "BQ", "SNOWFLAKE", "POSTGRES", "CLICKHOUSE"}
	jobQueryBatchSize = config.GetInt("Router.jobQueryBatchSize", 10000)
	noOfWorkers = config.GetInt("Warehouse.noOfWorkers", 8)
	noOfSlaveWorkerRoutines = config.GetInt("Warehouse.noOfSlaveWorkerRoutines", 4)
	stagingFilesBatchSize = config.GetInt("Warehouse.stagingFilesBatchSize", 240)
	uploadFreqInS = config.GetInt64("Warehouse.uploadFreqInS", 1800)
	mainLoopSleep = config.GetDuration("Warehouse.mainLoopSleepInS", 60) * time.Second
	workerRetrySleep = config.GetDuration("Warehouse.workerRetrySleepInS", 5) * time.Second
	crashRecoverWarehouses = []string{"RS"}
	inProgressMap = map[string]bool{}
	inRecoveryMap = map[string]bool{}
	lastExecMap = map[string]int64{}
	warehouseMode = config.GetString("Warehouse.mode", "embedded")
	host = config.GetEnv("WAREHOUSE_JOBS_DB_HOST", "localhost")
	user = config.GetEnv("WAREHOUSE_JOBS_DB_USER", "ubuntu")
	dbname = config.GetEnv("WAREHOUSE_JOBS_DB_DB_NAME", "ubuntu")
	port, _ = strconv.Atoi(config.GetEnv("WAREHOUSE_JOBS_DB_PORT", "5432"))
	password = config.GetEnv("WAREHOUSE_JOBS_DB_PASSWORD", "ubuntu") // Reading secrets from
	sslmode = config.GetEnv("WAREHOUSE_JOBS_DB_SSL_MODE", "disable")
	warehouseSyncPreFetchCount = config.GetInt("Warehouse.warehouseSyncPreFetchCount", 10)
	stagingFilesSchemaPaginationSize = config.GetInt("Warehouse.stagingFilesSchemaPaginationSize", 100)
	warehouseSyncFreqIgnore = config.GetBool("Warehouse.warehouseSyncFreqIgnore", false)
	preLoadedIdentitiesMap = map[string]bool{}
}

// get name of the worker (`destID_namespace`) to be stored in map wh.workerChannelMap
func workerIdentifier(warehouse warehouseutils.WarehouseT) string {
	return fmt.Sprintf(`%s_%s`, warehouse.Destination.ID, warehouse.Namespace)
}

func (wh *HandleT) handleUploadJobs(processStagingFilesJobList []ProcessStagingFilesJobT) {
	// infinite loop to check for active workers count and retry if not
	// break after handling
	for {
		// check number of workers actively enagaged
		// if limit hit, sleep and check again
		// activeWorkerCount is across all wh.destType's
		activeWorkerCountLock.Lock()
		activeWorkers := activeWorkerCount
		if activeWorkers >= noOfWorkers {
			activeWorkerCountLock.Unlock()
			logger.Debugf("[WH]: Setting to sleep and waiting till activeWorkers are less than %d", noOfWorkers)
			// TODO: add randomness to this ?
			time.Sleep(workerRetrySleep)
			continue
		}

		// increment number of workers actively engaged
		activeWorkerCount++
		activeWorkerCountLock.Unlock()

		// START: processing of upload job

		whOneFullPassTimer := warehouseutils.DestStat(stats.TimerType, "total_end_to_end_step_time", processStagingFilesJobList[0].Warehouse.Destination.ID)
		whOneFullPassTimer.Start()
		for _, job := range processStagingFilesJobList {
			if len(job.List) == 0 {
				warehouseutils.DestStat(stats.CountType, "failed_uploads", job.Warehouse.Destination.ID).Count(1)
				warehouseutils.SetUploadError(job.Upload, errors.New("no staging files found"), warehouseutils.GeneratingLoadFileFailedState, wh.dbHandle)
				wh.recordDeliveryStatus(job.Warehouse.Destination.ID, job.Upload.ID)
				break
			}
			// consolidate schema if not already done
			schemaInDB, err := warehouseutils.GetCurrentSchema(wh.dbHandle, job.Warehouse)
			whManager, err := warehousemanager.NewWhManager(wh.destType)
			if err != nil {
				panic(err)
			}

			warehouseutils.SetUploadStatus(job.Upload, warehouseutils.FetchingSchemaState, wh.dbHandle)
			syncedSchema, err := whManager.FetchSchema(job.Warehouse, job.Warehouse.Namespace)
			if err != nil {
				logger.Errorf(`WH: Failed fetching schema from warehouse: %v`, err)
				warehouseutils.DestStat(stats.CountType, "failed_uploads", job.Warehouse.Destination.ID).Count(1)
				warehouseutils.SetUploadError(job.Upload, err, warehouseutils.FetchingSchemaFailedState, wh.dbHandle)
				wh.recordDeliveryStatus(job.Warehouse.Destination.ID, job.Upload.ID)
				break
			}

			hasSchemaChanged := !warehouseutils.CompareSchema(schemaInDB, syncedSchema)
			if hasSchemaChanged {
				err = warehouseutils.UpdateCurrentSchema(job.Warehouse.Namespace, job.Warehouse, job.Upload.ID, syncedSchema, wh.dbHandle)
				if err != nil {
					panic(err)
				}
			}

			if len(job.Upload.Schema) == 0 || hasSchemaChanged {
				// merge schemas over all staging files in this batch
				logger.Infof("[WH]: Consolidating upload schema with schema in wh_schemas...")
				consolidatedSchema := wh.consolidateSchema(job.Warehouse, job.List)
				marshalledSchema, err := json.Marshal(consolidatedSchema)
				if err != nil {
					panic(err)
				}
				warehouseutils.SetUploadColumns(
					job.Upload,
					wh.dbHandle,
					warehouseutils.UploadColumnT{Column: warehouseutils.UploadSchemaField, Value: marshalledSchema},
				)
				job.Upload.Schema = consolidatedSchema
			}
			if !wh.areTableUploadsCreated(job.Upload) {
				err := wh.initTableUploads(job.Upload, job.Upload.Schema)
				if err != nil {
					// TODO: Handle error / Retry
					logger.Error("[WH]: Error creating records in wh_table_uploads", err)
				}
			}

			createPlusUploadTimer := warehouseutils.DestStat(stats.TimerType, "stagingfileset_total_handling_time", job.Warehouse.Destination.ID)
			createPlusUploadTimer.Start()

			// generate load files only if not done before
			// upload records have start_load_file_id and end_load_file_id set to 0 on creation
			// and are updated on creation of load files
			logger.Infof("[WH]: Processing %d staging files in upload job:%v with staging files from %v to %v for %s:%s", len(job.List), job.Upload.ID, job.List[0].ID, job.List[len(job.List)-1].ID, wh.destType, job.Warehouse.Destination.ID)
			warehouseutils.SetUploadColumns(
				job.Upload,
				wh.dbHandle,
				warehouseutils.UploadColumnT{Column: warehouseutils.UploadLastExecAtField, Value: timeutil.Now()},
			)
			if job.Upload.StartLoadFileID == 0 || hasSchemaChanged {
				warehouseutils.SetUploadStatus(job.Upload, warehouseutils.GeneratingLoadFileState, wh.dbHandle)
				err := wh.createLoadFiles(&job)
				if err != nil {
					//Unreachable code. So not modifying the stat 'failed_uploads', which is reused later for copying.
					warehouseutils.DestStat(stats.CountType, "failed_uploads", job.Warehouse.Destination.ID).Count(1)
					warehouseutils.SetUploadError(job.Upload, err, warehouseutils.GeneratingLoadFileFailedState, wh.dbHandle)
					wh.recordDeliveryStatus(job.Warehouse.Destination.ID, job.Upload.ID)
					break
				}
			}
			err = wh.SyncLoadFilesToWarehouse(&job)
			wh.recordDeliveryStatus(job.Warehouse.Destination.ID, job.Upload.ID)

			createPlusUploadTimer.End()

			if err != nil {
				warehouseutils.DestStat(stats.CountType, "failed_uploads", job.Warehouse.Destination.ID).Count(1)
				break
			}
			onSuccessfulUpload(job.Warehouse)
			warehouseutils.DestStat(stats.CountType, "load_staging_files_into_warehouse", job.Warehouse.Destination.ID).Count(len(job.List))
		}
		if whOneFullPassTimer != nil {
			whOneFullPassTimer.End()
		}
		setDestInProgress(processStagingFilesJobList[0].Warehouse, false)

		// END: processing of upload job

		// decrement number of workers actively engaged
		activeWorkerCountLock.Lock()
		activeWorkerCount--
		activeWorkerCountLock.Unlock()

		break
	}
}

func (wh *HandleT) initWorker(identifier string) chan []ProcessStagingFilesJobT {
	workerChan := make(chan []ProcessStagingFilesJobT, 100)
	rruntime.Go(func() {
		for {
			processStagingFilesJobList := <-workerChan
			wh.handleUploadJobs(processStagingFilesJobList)
		}
	})
	return workerChan
}

func (wh *HandleT) backendConfigSubscriber() {
	ch := make(chan utils.DataEvent)
	backendconfig.Subscribe(ch, backendconfig.TopicBackendConfig)
	for {
		config := <-ch
		configSubscriberLock.Lock()
		wh.warehouses = []warehouseutils.WarehouseT{}
		allSources := config.Data.(backendconfig.SourcesT)

		for _, source := range allSources.Sources {
			if len(source.Destinations) > 0 {
				for _, destination := range source.Destinations {
					if destination.DestinationDefinition.Name == wh.destType {
						namespace := wh.getNamespace(destination.Config, source, destination, wh.destType)
						warehouse := warehouseutils.WarehouseT{Source: source, Destination: destination, Namespace: namespace}
						wh.warehouses = append(wh.warehouses, warehouse)

						workerName := workerIdentifier(warehouse)
						wh.workerChannelMapLock.Lock()
						// spawn one worker for each unique destID_namespace
						// check this commit to https://github.com/rudderlabs/rudder-server/pull/476/commits/4a0a10e5faa2c337c457f14c3ad1c32e2abfb006
						// to avoid creating goroutine for disabled sources/destiantions
						if _, ok := wh.workerChannelMap[workerName]; !ok {
							workerChan := wh.initWorker(workerName)
							wh.workerChannelMap[workerName] = workerChan
						}
						wh.workerChannelMapLock.Unlock()

						if destination.Config != nil && destination.Enabled && destination.Config["eventDelivery"] == true {
							sourceID := source.ID
							destinationID := destination.ID
							rruntime.Go(func() {
								wh.syncLiveWarehouseStatus(sourceID, destinationID)
							})
						}
						if val, ok := destination.Config["testConnection"].(bool); ok && val {
							destination := destination
							rruntime.Go(func() {
								testResponse := destinationConnectionTester.TestWarehouseDestinationConnection(destination)
								destinationConnectionTester.UploadDestinationConnectionTesterResponse(testResponse, destination.ID)
							})
						}
					}
				}
			}
		}
		configSubscriberLock.Unlock()
	}
}

// getNamespace sets namespace name in the following order
// 	1. user set name from destinationConfig
// 	2. from existing record in wh_schemas with same source + dest combo
// 	3. convert source name
func (wh *HandleT) getNamespace(config interface{}, source backendconfig.SourceT, destination backendconfig.DestinationT, destType string) string {
	configMap := config.(map[string]interface{})
	var namespace string
	if destType == "CLICKHOUSE" {
		//TODO: Handle if configMap["database"] is nil
		return configMap["database"].(string)
	}
	if configMap["namespace"] != nil {
		namespace = configMap["namespace"].(string)
		if len(strings.TrimSpace(namespace)) > 0 {
			return warehouseutils.ToProviderCase(destType, warehouseutils.ToSafeNamespace(destType, namespace))
		}
	}
	var exists bool
	if namespace, exists = warehouseutils.GetNamespace(source, destination, wh.dbHandle); !exists {
		namespace = warehouseutils.ToProviderCase(destType, warehouseutils.ToSafeNamespace(destType, source.Name))
	}
	return namespace
}

func (wh *HandleT) getStagingFiles(warehouse warehouseutils.WarehouseT, startID int64, endID int64) ([]*StagingFileT, error) {
	sqlStatement := fmt.Sprintf(`SELECT id, location
                                FROM %[1]s
								WHERE %[1]s.id >= %[2]v AND %[1]s.id <= %[3]v AND %[1]s.source_id='%[4]s' AND %[1]s.destination_id='%[5]s'
								ORDER BY id ASC`,
		warehouseutils.WarehouseStagingFilesTable, startID, endID, warehouse.Source.ID, warehouse.Destination.ID)
	rows, err := wh.dbHandle.Query(sqlStatement)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
	defer rows.Close()

	var stagingFilesList []*StagingFileT
	for rows.Next() {
		var jsonUpload StagingFileT
		err := rows.Scan(&jsonUpload.ID, &jsonUpload.Location)
		if err != nil {
			panic(err)
		}
		stagingFilesList = append(stagingFilesList, &jsonUpload)
	}

	return stagingFilesList, nil
}

func (wh *HandleT) getPendingStagingFiles(warehouse warehouseutils.WarehouseT) ([]*StagingFileT, error) {
	var lastStagingFileID int64
	sqlStatement := fmt.Sprintf(`SELECT end_staging_file_id FROM %[1]s WHERE %[1]s.source_id='%[2]s' AND %[1]s.destination_id='%[3]s' AND (%[1]s.status= '%[4]s' OR %[1]s.status = '%[5]s') ORDER BY %[1]s.id DESC`, warehouseutils.WarehouseUploadsTable, warehouse.Source.ID, warehouse.Destination.ID, warehouseutils.ExportedDataState, warehouseutils.AbortedState)
	err := wh.dbHandle.QueryRow(sqlStatement).Scan(&lastStagingFileID)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}

	sqlStatement = fmt.Sprintf(`SELECT id, location, first_event_at, last_event_at
                                FROM %[1]s
								WHERE %[1]s.id > %[2]v AND %[1]s.source_id='%[3]s' AND %[1]s.destination_id='%[4]s'
								ORDER BY id ASC`,
		warehouseutils.WarehouseStagingFilesTable, lastStagingFileID, warehouse.Source.ID, warehouse.Destination.ID)
	rows, err := wh.dbHandle.Query(sqlStatement)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
	defer rows.Close()

	var stagingFilesList []*StagingFileT
	var firstEventAt, lastEventAt sql.NullTime
	for rows.Next() {
		var jsonUpload StagingFileT
		err := rows.Scan(&jsonUpload.ID, &jsonUpload.Location, &firstEventAt, &lastEventAt)
		if err != nil {
			panic(err)
		}
		jsonUpload.FirstEventAt = firstEventAt.Time
		jsonUpload.LastEventAt = lastEventAt.Time
		stagingFilesList = append(stagingFilesList, &jsonUpload)
	}

	return stagingFilesList, nil
}

func (wh *HandleT) areTableUploadsCreated(upload warehouseutils.UploadT) bool {
	sqlStatement := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE wh_upload_id=%d`, warehouseutils.WarehouseTableUploadsTable, upload.ID)
	var count int
	err := wh.dbHandle.QueryRow(sqlStatement).Scan(&count)
	if err != nil {
		panic(err)
	}
	return count > 0
}

func (wh *HandleT) initTableUploads(upload warehouseutils.UploadT, schema map[string]map[string]string) (err error) {

	//Using transactions for bulk copying
	txn, err := wh.dbHandle.Begin()
	if err != nil {
		return
	}

	stmt, err := txn.Prepare(pq.CopyIn(warehouseutils.WarehouseTableUploadsTable, "wh_upload_id", "table_name", "status", "error", "created_at", "updated_at"))
	if err != nil {
		return
	}

	tables := make([]string, 0, len(schema))
	for t := range schema {
		tables = append(tables, t)
		// also track upload to rudder_identity_mappings if the upload has records for rudder_identity_merge_rules
		if misc.ContainsString(warehouseutils.IdentityEnabledWarehouses, wh.destType) && t == warehouseutils.ToProviderCase(wh.destType, warehouseutils.IdentityMergeRulesTable) {
			if _, ok := schema[warehouseutils.ToProviderCase(wh.destType, warehouseutils.IdentityMappingsTable)]; !ok {
				tables = append(tables, warehouseutils.ToProviderCase(upload.DestinationType, warehouseutils.IdentityMappingsTable))
			}
		}
	}

	now := timeutil.Now()
	for _, table := range tables {
		_, err = stmt.Exec(upload.ID, table, "waiting", "{}", now, now)
		if err != nil {
			return
		}
	}

	_, err = stmt.Exec()
	if err != nil {
		return
	}

	err = stmt.Close()
	if err != nil {
		return
	}

	err = txn.Commit()
	if err != nil {
		return
	}
	return
}

func (wh *HandleT) initUpload(warehouse warehouseutils.WarehouseT, jsonUploadsList []*StagingFileT) warehouseutils.UploadT {
	sqlStatement := fmt.Sprintf(`INSERT INTO %s (source_id, namespace, destination_id, destination_type, start_staging_file_id, end_staging_file_id, start_load_file_id, end_load_file_id, status, schema, error, first_event_at, last_event_at, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6 ,$7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`, warehouseutils.WarehouseUploadsTable)
	logger.Infof("[WH]: %s: Creating record in %s table: %v", wh.destType, warehouseutils.WarehouseUploadsTable, sqlStatement)
	stmt, err := wh.dbHandle.Prepare(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer stmt.Close()

	startJSONID := jsonUploadsList[0].ID
	endJSONID := jsonUploadsList[len(jsonUploadsList)-1].ID
	namespace := warehouse.Namespace

	var firstEventAt, lastEventAt time.Time
	if ok := jsonUploadsList[0].FirstEventAt.IsZero(); !ok {
		firstEventAt = jsonUploadsList[0].FirstEventAt
	}
	if ok := jsonUploadsList[len(jsonUploadsList)-1].LastEventAt.IsZero(); !ok {
		lastEventAt = jsonUploadsList[len(jsonUploadsList)-1].LastEventAt
	}

	now := timeutil.Now()
	row := stmt.QueryRow(warehouse.Source.ID, namespace, warehouse.Destination.ID, wh.destType, startJSONID, endJSONID, 0, 0, warehouseutils.WaitingState, "{}", "{}", firstEventAt, lastEventAt, now, now)

	var uploadID int64
	err = row.Scan(&uploadID)
	if err != nil {
		panic(err)
	}

	upload := warehouseutils.UploadT{
		ID:                 uploadID,
		Namespace:          warehouse.Namespace,
		SourceID:           warehouse.Source.ID,
		DestinationID:      warehouse.Destination.ID,
		DestinationType:    wh.destType,
		StartStagingFileID: startJSONID,
		EndStagingFileID:   endJSONID,
		Status:             warehouseutils.WaitingState,
	}

	return upload
}

func (wh *HandleT) getPendingUploads(warehouse warehouseutils.WarehouseT) ([]warehouseutils.UploadT, bool) {

	sqlStatement := fmt.Sprintf(`SELECT id, status, schema, namespace, start_staging_file_id, end_staging_file_id, start_load_file_id, end_load_file_id, error, timings->0 as firstTiming, timings->-1 as lastTiming FROM %[1]s WHERE (%[1]s.source_id='%[2]s' AND %[1]s.destination_id='%[3]s' AND %[1]s.status!='%[4]s' AND %[1]s.status!='%[5]s') ORDER BY id asc`, warehouseutils.WarehouseUploadsTable, warehouse.Source.ID, warehouse.Destination.ID, warehouseutils.ExportedDataState, warehouseutils.AbortedState)

	rows, err := wh.dbHandle.Query(sqlStatement)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
	if err == sql.ErrNoRows {
		return []warehouseutils.UploadT{}, false
	}
	defer rows.Close()

	var uploads []warehouseutils.UploadT
	for rows.Next() {
		var upload warehouseutils.UploadT
		var schema json.RawMessage
		var firstTiming sql.NullString
		var lastTiming sql.NullString
		err := rows.Scan(&upload.ID, &upload.Status, &schema, &upload.Namespace, &upload.StartStagingFileID, &upload.EndStagingFileID, &upload.StartLoadFileID, &upload.EndLoadFileID, &upload.Error, &firstTiming, &lastTiming)
		if err != nil {
			panic(err)
		}
		upload.Schema = warehouseutils.JSONSchemaToMap(schema)

		_, upload.FirstAttemptAt = warehouseutils.TimingFromJSONString(firstTiming)
		var lastStatus string
		lastStatus, upload.LastAttemptAt = warehouseutils.TimingFromJSONString(lastTiming)
		upload.Attempts = gjson.Get(string(upload.Error), fmt.Sprintf(`%s.attempt`, lastStatus)).Int()

		uploads = append(uploads, upload)
	}

	var anyPending bool
	if len(uploads) > 0 {
		anyPending = true
	}
	return uploads, anyPending
}

func connectionString(warehouse warehouseutils.WarehouseT) string {
	return fmt.Sprintf(`source:%s:destination:%s`, warehouse.Source.ID, warehouse.Destination.ID)
}

func setDestInProgress(warehouse warehouseutils.WarehouseT, starting bool) {
	inProgressMapLock.Lock()
	if starting {
		inProgressMap[connectionString(warehouse)] = true
	} else {
		delete(inProgressMap, connectionString(warehouse))
	}
	inProgressMapLock.Unlock()
}

func isDestInProgress(warehouse warehouseutils.WarehouseT) bool {
	inProgressMapLock.RLock()
	if inProgressMap[connectionString(warehouse)] {
		inProgressMapLock.RUnlock()
		return true
	}
	inProgressMapLock.RUnlock()
	return false
}

func uploadFrequencyExceeded(warehouse warehouseutils.WarehouseT, syncFrequency string) bool {
	freqInS := uploadFreqInS
	if syncFrequency != "" {
		freqInMin, _ := strconv.ParseInt(syncFrequency, 10, 64)
		freqInS = freqInMin * 60
	}
	lastExecMapLock.Lock()
	defer lastExecMapLock.Unlock()
	if lastExecTime, ok := lastExecMap[connectionString(warehouse)]; ok && timeutil.Now().Unix()-lastExecTime < freqInS {
		return true
	}
	return false
}

func setLastExec(warehouse warehouseutils.WarehouseT) {
	lastExecMapLock.Lock()
	defer lastExecMapLock.Unlock()
	lastExecMap[connectionString(warehouse)] = timeutil.Now().Unix()
}

func (wh *HandleT) mainLoop() {
	for {
		wh.configSubscriberLock.RLock()
		if !wh.isEnabled {
			wh.configSubscriberLock.RUnlock()
			time.Sleep(mainLoopSleep)
			continue
		}
		wh.configSubscriberLock.RUnlock()

		configSubscriberLock.RLock()
		warehouses := wh.warehouses
		configSubscriberLock.RUnlock()
		for _, warehouse := range warehouses {
			if isDestInProgress(warehouse) {
				logger.Debugf("[WH]: Skipping upload loop since %s:%s upload in progress", wh.destType, warehouse.Destination.ID)
				continue
			}
			setDestInProgress(warehouse, true)

			// ---- start: check and preload local identity tables with existing data from warehouse -----
			if warehouseutils.IDResolutionEnabled() && misc.ContainsString(warehouseutils.IdentityEnabledWarehouses, wh.destType) {
				if !warehouse.Destination.Enabled {
					continue
				}

				if isDestPreLoaded(warehouse) {
					continue
				}

				wh.setupIdentityTables(warehouse)

				// check if identity tables have records locally and
				// check if warehouse has data
				if !wh.hasLocalIdentityData(warehouse) {
					hasData, err := wh.hasWarehouseData(warehouse)
					if err != nil {
						logger.Errorf(`WH: Error checking for data in %s:%s:%s`, wh.destType, warehouse.Destination.ID, warehouse.Destination.Name)
						warehouseutils.DestStat(stats.CountType, "failed_uploads", warehouse.Destination.ID).Count(1)
						continue
					}
					if hasData {
						logger.Infof("[WH]: Did not find local identity tables..")
						logger.Infof("[WH]: Generating identity tables based on data in warehouse %s:%s", wh.destType, warehouse.Destination.ID)
						// TODO: make this async and not block other warehouses
						var upload warehouseutils.UploadT
						upload, err = wh.preLoadIdentityTables(warehouse)
						if err != nil {
							warehouseutils.DestStat(stats.CountType, "failed_uploads", warehouse.Destination.ID).Count(1)
						} else {
							setDestPreLoaded(warehouse)
						}
						wh.recordDeliveryStatus(warehouse.Destination.ID, upload.ID)
						setDestInProgress(warehouse, false)
						continue
					}
				}
			}
			// ---- end: check and preload local identity tables with existing data from warehouse -----

			_, ok := inRecoveryMap[warehouse.Destination.ID]
			if ok {
				whManager, err := warehousemanager.NewWhManager(wh.destType)
				if err != nil {
					panic(err)
				}
				logger.Infof("[WH]: Crash recovering for %s:%s", wh.destType, warehouse.Destination.ID)
				err = whManager.CrashRecover(warehouseutils.ConfigT{
					DbHandle:  wh.dbHandle,
					Warehouse: warehouse,
				})
				if err != nil {
					setDestInProgress(warehouse, false)
					continue
				}
				delete(inRecoveryMap, warehouse.Destination.ID)
			}
			// fetch any pending wh_uploads records (query for not successful/aborted uploads)
			pendingUploads, ok := wh.getPendingUploads(warehouse)
			if ok {
				logger.Infof("[WH]: Found pending uploads: %v for %s:%s", len(pendingUploads), wh.destType, warehouse.Destination.ID)
				jobs := []ProcessStagingFilesJobT{}
				for _, pendingUpload := range pendingUploads {
					if !wh.canStartPendingUpload(pendingUpload, warehouse) {
						logger.Debugf("[WH]: Skipping pending upload for %s:%s since current time less than next retry time", wh.destType, warehouse.Destination.ID)
						setDestInProgress(warehouse, false)
						break
					}
					stagingFilesList, err := wh.getStagingFiles(warehouse, pendingUpload.StartStagingFileID, pendingUpload.EndStagingFileID)
					if err != nil {
						panic(err)
					}
					job := ProcessStagingFilesJobT{
						List:      stagingFilesList,
						Warehouse: warehouse,
						Upload:    pendingUpload,
					}
					logger.Debugf("[WH]: Adding job %+v", job)
					jobs = append(jobs, job)
				}
				if len(jobs) == 0 {
					continue
				}
				wh.enqueueUploadJobs(jobs, warehouse)
			} else {
				if !wh.canStartUpload(warehouse) {
					logger.Debugf("[WH]: Skipping upload loop since %s:%s upload freq not exceeded", wh.destType, warehouse.Destination.ID)
					setDestInProgress(warehouse, false)
					continue
				}
				setLastExec(warehouse)
				// fetch staging files that are not processed yet
				stagingFilesList, err := wh.getPendingStagingFiles(warehouse)
				if err != nil {
					panic(err)
				}
				if len(stagingFilesList) == 0 {
					logger.Debugf("[WH]: Found no pending staging files for %s:%s", wh.destType, warehouse.Destination.ID)
					setDestInProgress(warehouse, false)
					continue
				}
				logger.Infof("[WH]: Found %v pending staging files for %s:%s", len(stagingFilesList), wh.destType, warehouse.Destination.ID)

				count := 0
				var jobs []ProcessStagingFilesJobT
				// process staging files in batches of stagingFilesBatchSize
				for {
					lastIndex := count + stagingFilesBatchSize
					if lastIndex >= len(stagingFilesList) {
						lastIndex = len(stagingFilesList)
					}
					upload := wh.initUpload(warehouse, stagingFilesList[count:lastIndex])
					job := ProcessStagingFilesJobT{
						List:      stagingFilesList[count:lastIndex],
						Warehouse: warehouse,
						Upload:    upload,
					}
					logger.Debugf("[WH]: Adding job %+v", job)
					jobs = append(jobs, job)
					count += stagingFilesBatchSize
					if count >= len(stagingFilesList) {
						break
					}
				}
				wh.enqueueUploadJobs(jobs, warehouse)
			}
		}
		time.Sleep(mainLoopSleep)
	}
}

func (wh *HandleT) enqueueUploadJobs(jobs []ProcessStagingFilesJobT, warehouse warehouseutils.WarehouseT) {
	workerName := workerIdentifier(warehouse)
	wh.workerChannelMapLock.Lock()
	wh.workerChannelMap[workerName] <- jobs
	wh.workerChannelMapLock.Unlock()
}

func (wh *HandleT) createLoadFiles(job *ProcessStagingFilesJobT) (err error) {
	// stat for time taken to process staging files in a single job
	timer := warehouseutils.DestStat(stats.TimerType, "process_staging_files_batch_time", job.Warehouse.Destination.ID)
	timer.Start()

	var jsonIDs []int64
	for _, job := range job.List {
		jsonIDs = append(jsonIDs, job.ID)
	}
	warehouseutils.SetStagingFilesStatus(jsonIDs, warehouseutils.StagingFileExecutingState, wh.dbHandle)

	logger.Debugf("[WH]: Starting batch processing %v stage files with %v workers for %s:%s", len(job.List), noOfWorkers, wh.destType, job.Warehouse.Destination.ID)
	var messages []pgnotifier.MessageT
	for _, stagingFile := range job.List {
		payload := PayloadT{
			UploadID:            job.Upload.ID,
			StagingFileID:       stagingFile.ID,
			StagingFileLocation: stagingFile.Location,
			Schema:              job.Upload.Schema,
			SourceID:            job.Warehouse.Source.ID,
			DestinationID:       job.Warehouse.Destination.ID,
			DestinationType:     wh.destType,
			DestinationConfig:   job.Warehouse.Destination.Config,
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		message := pgnotifier.MessageT{
			Payload: payloadJSON,
		}
		messages = append(messages, message)
	}

	logger.Infof("[WH]: Publishing %d staging files to PgNotifier", len(messages))
	var loadFileIDs []int64
	ch, err := wh.notifier.Publish("process_staging_file", messages)
	if err != nil {
		panic(err)
	}

	responses := <-ch
	logger.Infof("[WH]: Received responses from PgNotifier")
	for _, resp := range responses {
		// TODO: make it aborted
		if resp.Status == "aborted" {
			logger.Errorf("Error in genrating load files: %v", resp.Error)
			continue
		}
		var payload map[string]interface{}
		err = json.Unmarshal(resp.Payload, &payload)
		if err != nil {
			panic(err)
		}
		respIDs, ok := payload["LoadFileIDs"].([]interface{})
		if !ok {
			logger.Errorf("No LoadFIleIDS returned by wh worker")
			continue
		}
		ids := make([]int64, len(respIDs))
		for i := range respIDs {
			ids[i] = int64(respIDs[i].(float64))
		}
		loadFileIDs = append(loadFileIDs, ids...)
	}

	timer.End()
	if len(loadFileIDs) == 0 {
		err = errors.New(responses[0].Error)
		warehouseutils.SetStagingFilesError(jsonIDs, warehouseutils.StagingFileFailedState, wh.dbHandle, err)
		warehouseutils.DestStat(stats.CountType, "process_staging_files_failures", job.Warehouse.Destination.ID).Count(len(job.List))
		return err
	}
	warehouseutils.SetStagingFilesStatus(jsonIDs, warehouseutils.StagingFileSucceededState, wh.dbHandle)
	warehouseutils.DestStat(stats.CountType, "process_staging_files_success", job.Warehouse.Destination.ID).Count(len(job.List))
	warehouseutils.DestStat(stats.CountType, "generate_load_files", job.Warehouse.Destination.ID).Count(len(loadFileIDs))

	sort.Slice(loadFileIDs, func(i, j int) bool { return loadFileIDs[i] < loadFileIDs[j] })
	startLoadFileID := loadFileIDs[0]
	endLoadFileID := loadFileIDs[len(loadFileIDs)-1]

	err = warehouseutils.SetUploadStatus(
		job.Upload,
		warehouseutils.GeneratedLoadFileState,
		wh.dbHandle,
		warehouseutils.UploadColumnT{Column: warehouseutils.UploadStartLoadFileIDField, Value: startLoadFileID},
		warehouseutils.UploadColumnT{Column: warehouseutils.UploadEndLoadFileIDField, Value: endLoadFileID},
	)
	if err != nil {
		panic(err)
	}

	job.Upload.StartLoadFileID = startLoadFileID
	job.Upload.EndLoadFileID = endLoadFileID
	job.Upload.Status = warehouseutils.GeneratedLoadFileState
	return
}

func (wh *HandleT) updateTableEventsCount(tableName string, upload warehouseutils.UploadT, warehouse warehouseutils.WarehouseT) (err error) {
	subQuery := fmt.Sprintf(`SELECT sum(total_events) as total from %[1]s right join (
		SELECT  staging_file_id, MAX(id) AS id FROM wh_load_files
		WHERE ( source_id='%[2]s'
			AND destination_id='%[3]s'
			AND table_name='%[4]s'
			AND id >= %[5]v
			AND id <= %[6]v)
		GROUP BY staging_file_id ) uniqueStagingFiles
		ON  wh_load_files.id = uniqueStagingFiles.id `,
		warehouseutils.WarehouseLoadFilesTable,
		warehouse.Source.ID,
		warehouse.Destination.ID,
		tableName,
		upload.StartLoadFileID,
		upload.EndLoadFileID,
		warehouseutils.WarehouseTableUploadsTable)

	sqlStatement := fmt.Sprintf(`update %[1]s set total_events = subquery.total FROM (%[2]s) AS subquery WHERE table_name = '%[3]s' AND wh_upload_id = %[4]d`,
		warehouseutils.WarehouseTableUploadsTable,
		subQuery,
		tableName,
		upload.ID)
	_, err = wh.dbHandle.Exec(sqlStatement)
	return
}

func (wh *HandleT) SyncLoadFilesToWarehouse(job *ProcessStagingFilesJobT) (err error) {
	for tableName := range job.Upload.Schema {
		err := wh.updateTableEventsCount(tableName, job.Upload, job.Warehouse)
		if err != nil {
			panic(err)
		}
	}
	logger.Infof("[WH]: Starting load flow for %s:%s", wh.destType, job.Warehouse.Destination.ID)
	whManager, err := warehousemanager.NewWhManager(wh.destType)
	if err != nil {
		panic(err)
	}
	err = whManager.Process(warehouseutils.ConfigT{
		DbHandle:  wh.dbHandle,
		Upload:    job.Upload,
		Warehouse: job.Warehouse,
	})
	return
}

func getBucketFolder(batchID string, tableName string) string {
	return fmt.Sprintf(`%v-%v`, batchID, tableName)
}

//Enable enables a router :)
func (wh *HandleT) Enable() {
	wh.isEnabled = true
}

//Disable disables a router:)
func (wh *HandleT) Disable() {
	wh.isEnabled = false
}

func (wh *HandleT) setInterruptedDestinations() (err error) {
	if !misc.Contains(crashRecoverWarehouses, wh.destType) {
		return
	}
	sqlStatement := fmt.Sprintf(`SELECT destination_id FROM %s WHERE destination_type='%s' AND (status='%s' OR status='%s')`, warehouseutils.WarehouseUploadsTable, wh.destType, warehouseutils.ExportingDataState, warehouseutils.ExportingDataFailedState)
	rows, err := wh.dbHandle.Query(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	for rows.Next() {
		var destID string
		err := rows.Scan(&destID)
		if err != nil {
			panic(err)
		}
		inRecoveryMap[destID] = true
	}
	return err
}

func (wh *HandleT) Setup(whType string) {
	logger.Infof("[WH]: Warehouse Router started: %s", whType)
	wh.dbHandle = dbHandle
	wh.notifier = notifier
	wh.destType = whType
	wh.setInterruptedDestinations()
	wh.Enable()
	wh.uploadToWarehouseQ = make(chan []ProcessStagingFilesJobT)
	wh.createLoadFilesQ = make(chan LoadFileJobT)
	wh.workerChannelMap = make(map[string]chan []ProcessStagingFilesJobT)
	rruntime.Go(func() {
		wh.backendConfigSubscriber()
	})
	rruntime.Go(func() {
		wh.mainLoop()
	})
}

var loadFileFormatMap = map[string]string{
	"BQ":         "json",
	"RS":         "csv",
	"SNOWFLAKE":  "csv",
	"POSTGRES":   "csv",
	"CLICKHOUSE": "csv",
}

// Gets the config from config backend and extracts enabled writekeys
func monitorDestRouters() {
	ch := make(chan utils.DataEvent)
	backendconfig.Subscribe(ch, backendconfig.TopicBackendConfig)
	dstToWhRouter := make(map[string]*HandleT)

	for {
		config := <-ch
		logger.Debug("Got config from config-backend", config)
		sources := config.Data.(backendconfig.SourcesT)
		enabledDestinations := make(map[string]bool)
		for _, source := range sources.Sources {
			for _, destination := range source.Destinations {
				enabledDestinations[destination.DestinationDefinition.Name] = true
				if misc.Contains(WarehouseDestinations, destination.DestinationDefinition.Name) {
					wh, ok := dstToWhRouter[destination.DestinationDefinition.Name]
					if !ok {
						logger.Info("Starting a new Warehouse Destination Router: ", destination.DestinationDefinition.Name)
						var wh HandleT
						wh.configSubscriberLock.Lock()
						wh.Setup(destination.DestinationDefinition.Name)
						wh.configSubscriberLock.Unlock()
						dstToWhRouter[destination.DestinationDefinition.Name] = &wh
					} else {
						logger.Debug("Enabling existing Destination: ", destination.DestinationDefinition.Name)
						wh.configSubscriberLock.Lock()
						wh.Enable()
						wh.configSubscriberLock.Unlock()
					}
				}
			}
		}

		keys := misc.StringKeys(dstToWhRouter)
		for _, key := range keys {
			if _, ok := enabledDestinations[key]; !ok {
				if whHandle, ok := dstToWhRouter[key]; ok {
					logger.Info("Disabling a existing warehouse destination: ", key)
					whHandle.configSubscriberLock.Lock()
					whHandle.Disable()
					whHandle.configSubscriberLock.Unlock()
				}
			}
		}
	}
}

func setupTables(dbHandle *sql.DB) {
	m := &migrator.Migrator{
		Handle:          dbHandle,
		MigrationsTable: "wh_schema_migrations",
	}

	err := m.Migrate("warehouse")
	if err != nil {
		panic(fmt.Errorf("Could not run warehouse database migrations: %w", err))
	}
}

func CheckPGHealth() bool {
	rows, err := dbHandle.Query(fmt.Sprintf(`SELECT 'Rudder Warehouse DB Health Check'::text as message`))
	if err != nil {
		logger.Error(err)
		return false
	}
	defer rows.Close()
	return true
}

func processHandler(w http.ResponseWriter, r *http.Request) {
	logger.LogRequest(r)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var stagingFile warehouseutils.StagingFileT
	json.Unmarshal(body, &stagingFile)

	var firstEventAt, lastEventAt interface{}
	firstEventAt = stagingFile.FirstEventAt
	lastEventAt = stagingFile.LastEventAt
	if stagingFile.FirstEventAt == "" || stagingFile.LastEventAt == "" {
		firstEventAt = nil
		lastEventAt = nil
	}

	logger.Debugf("BRT: Creating record for uploaded json in %s table with schema: %+v", warehouseutils.WarehouseStagingFilesTable, stagingFile.Schema)
	schemaPayload, err := json.Marshal(stagingFile.Schema)
	sqlStatement := fmt.Sprintf(`INSERT INTO %s (location, schema, source_id, destination_id, status, total_events, first_event_at, last_event_at, created_at, updated_at)
									   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`, warehouseutils.WarehouseStagingFilesTable)
	stmt, err := dbHandle.Prepare(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer stmt.Close()

	_, err = stmt.Exec(stagingFile.Location, schemaPayload, stagingFile.BatchDestination.Source.ID, stagingFile.BatchDestination.Destination.ID, warehouseutils.StagingFileWaitingState, stagingFile.TotalEvents, firstEventAt, lastEventAt, timeutil.Now())
	if err != nil {
		panic(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	var dbService string = "UP"
	if !CheckPGHealth() {
		dbService = "DOWN"
	}
	healthVal := fmt.Sprintf(`{"server":"UP", "db":"%s","acceptingEvents":"TRUE","warehouseMode":"%s","goroutines":"%d"}`, dbService, strings.ToUpper(warehouseMode), runtime.NumGoroutine())
	w.Write([]byte(healthVal))
}

func getConnectionString() string {
	if warehouseMode == config.EmbeddedMode {
		return jobsdb.GetConnectionString()
	}
	return fmt.Sprintf("host=%s port=%d user=%s "+
		"password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode)
}

func startWebHandler() {
	// do not register same endpoint when running embedded in rudder backend
	if isStandAlone() {
		http.HandleFunc("/health", healthHandler)
	}
	if isMaster() {
		backendconfig.WaitForConfig()
		http.HandleFunc("/v1/process", processHandler)
		logger.Infof("[WH]: Starting warehouse master service in %d", webPort)
	} else {
		logger.Infof("[WH]: Starting warehouse slave service in %d", webPort)
	}
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(webPort), bugsnag.Handler(nil)))
}

func isStandAlone() bool {
	return warehouseMode != EmbeddedMode
}

func isMaster() bool {
	return warehouseMode == config.MasterMode || warehouseMode == config.MasterSlaveMode || warehouseMode == config.EmbeddedMode
}

func isSlave() bool {
	return warehouseMode == config.SlaveMode || warehouseMode == config.MasterSlaveMode || warehouseMode == config.EmbeddedMode
}

func Start() {
	time.Sleep(1 * time.Second)
	// do not start warehouse service if rudder core is not in normal mode and warehouse is running in same process as rudder core
	if !isStandAlone() && !db.IsNormalMode() {
		logger.Infof("Skipping start of warehouse service...")
		return
	}

	logger.Infof("[WH]: Starting Warehouse service...")
	var err error
	psqlInfo := getConnectionString()

	dbHandle, err = sql.Open("postgres", psqlInfo)
	if err != nil {
		panic(err)
	}

	if !validators.IsPostgresCompatible(dbHandle) {
		err := errors.New("Rudder Warehouse Service needs postgres version >= 10. Exiting")
		logger.Error(err)
		panic(err)
	}

	setupTables(dbHandle)

	notifier, err = pgnotifier.New(psqlInfo)
	if err != nil {
		panic(err)
	}

	if isSlave() {
		logger.Infof("[WH]: Starting warehouse slave...")
		setupSlave()
	}

	if isMaster() {
		if warehouseMode != config.EmbeddedMode {
			backendconfig.Setup(false, nil)
		}
		logger.Infof("[WH]: Starting warehouse master...")
		err = notifier.AddTopic("process_staging_file")
		if err != nil {
			panic(err)
		}
		rruntime.Go(func() {
			monitorDestRouters()
		})
	}

	startWebHandler()
}
