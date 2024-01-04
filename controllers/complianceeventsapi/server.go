package complianceeventsapi

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- for convenience, not cryptography
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

var (
	clusterKeyCache      sync.Map
	parentPolicyKeyCache sync.Map
	policyKeyCache       sync.Map
)

type complianceAPIServer struct {
	Lock      *sync.Mutex
	server    *http.Server
	addr      string
	isRunning bool
}

// Start starts the http server. If it is already running, it has no effect.
func (s *complianceAPIServer) Start(dbURL string) error {
	s.Lock.Lock()
	defer s.Lock.Unlock()

	if s.isRunning {
		return nil
	}

	mux := http.NewServeMux()

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: mux,

		// need to investigate ideal values for these
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return err
	}

	// register handlers here
	mux.HandleFunc("/api/v1/compliance-events", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postComplianceEvent(db, w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	go func() {
		err := s.server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Error(err, "Error starting compliance events api server")
		}
	}()

	s.isRunning = true

	return nil
}

// Stop stops the http server. If it is not currently running, it has no effect.
func (s *complianceAPIServer) Stop() error {
	s.Lock.Lock()
	defer s.Lock.Unlock()

	if !s.isRunning {
		return nil
	}

	if err := s.server.Shutdown(context.TODO()); err != nil {
		log.Error(err, "Error stopping compliance events api server")

		return err
	}

	s.isRunning = false

	return nil
}

func postComplianceEvent(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error(err, "error reading request body")
		writeErrMsgJSON(w, "Could not read request body", http.StatusBadRequest)

		return
	}

	reqEvent := &ComplianceEvent{}

	if err := json.Unmarshal(body, reqEvent); err != nil {
		writeErrMsgJSON(w, "Incorrectly formatted request body, must be valid JSON", http.StatusBadRequest)

		return
	}

	if err := reqEvent.Validate(); err != nil {
		writeErrMsgJSON(w, err.Error(), http.StatusBadRequest)

		return
	}

	clusterFK, err := getClusterForeignKey(r.Context(), db, reqEvent.Cluster)
	if err != nil {
		log.Error(err, "error getting cluster foreign key")
		writeErrMsgJSON(w, "Internal Error", http.StatusInternalServerError)

		return
	}

	reqEvent.Event.ClusterID = clusterFK

	if reqEvent.ParentPolicy != nil {
		pfk, err := getParentPolicyForeignKey(r.Context(), db, *reqEvent.ParentPolicy)
		if err != nil {
			log.Error(err, "error getting parent policy foreign key")
			writeErrMsgJSON(w, "Internal Error", http.StatusInternalServerError)

			return
		}

		reqEvent.Event.ParentPolicyID = &pfk
	}

	policyFK, err := getPolicyForeignKey(r.Context(), db, reqEvent.Policy)
	if err != nil {
		if errors.Is(err, errRequiredFieldNotProvided) {
			writeErrMsgJSON(w, err.Error(), http.StatusBadRequest)

			return
		}

		log.Error(err, "error getting policy foreign key")
		writeErrMsgJSON(w, "Internal Error", http.StatusInternalServerError)

		return
	}

	reqEvent.Event.PolicyID = policyFK

	err = reqEvent.Create(r.Context(), db)
	if err != nil {
		log.Error(err, "error inserting compliance event")
		writeErrMsgJSON(w, "Internal Error", http.StatusInternalServerError)

		return
	}

	// remove the spec to only respond with the specHash
	reqEvent.Policy.Spec = ""

	resp, err := json.Marshal(reqEvent)
	if err != nil {
		log.Error(err, "error marshaling reqEvent for the response")
		writeErrMsgJSON(w, "Internal Error", http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusCreated)

	if _, err = w.Write(resp); err != nil {
		log.Error(err, "error writing success response")
	}
}

func getClusterForeignKey(ctx context.Context, db *sql.DB, cluster Cluster) (int, error) {
	// Check cache
	key, ok := clusterKeyCache.Load(cluster.ClusterID)
	if ok {
		return key.(int), nil
	}

	err := cluster.GetOrCreate(ctx, db)
	if err != nil {
		return 0, err
	}

	clusterKeyCache.Store(cluster.ClusterID, cluster.KeyID)

	return cluster.KeyID, nil
}

func getParentPolicyForeignKey(ctx context.Context, db *sql.DB, parent ParentPolicy) (int, error) {
	// Check cache
	parKey := parent.key()

	key, ok := parentPolicyKeyCache.Load(parKey)
	if ok {
		return key.(int), nil
	}

	err := parent.GetOrCreate(ctx, db)
	if err != nil {
		return 0, err
	}

	parentPolicyKeyCache.Store(parKey, parent.KeyID)

	return parent.KeyID, nil
}

func getPolicyForeignKey(ctx context.Context, db *sql.DB, pol Policy) (int, error) {
	// Fill in missing fields that can be inferred from other fields
	if pol.SpecHash == "" {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(pol.Spec)); err != nil {
			return 0, err // This kind of error would have been found during validation
		}

		sum := sha1.Sum(buf.Bytes()) // #nosec G401 -- for convenience, not cryptography
		pol.SpecHash = hex.EncodeToString(sum[:])
	}

	// Check cache
	polKey := pol.key()

	key, ok := policyKeyCache.Load(polKey)
	if ok {
		return key.(int), nil
	}

	if pol.Spec == "" {
		row := db.QueryRowContext(
			ctx, "SELECT spec FROM policies WHERE spec_hash=$1 LIMIT 1", pol.SpecHash,
		)
		if row.Err() != nil {
			return 0, fmt.Errorf("could not determine the spec from the provided spec hash: %w", row.Err())
		}

		err := row.Scan(&pol.Spec)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf(
					"%w: could not determine the spec from the provided spec hash; the spec is required in the request",
					errRequiredFieldNotProvided,
				)
			}

			return 0, fmt.Errorf(
				"the database returned an unexpected spec value for the provided spec hash: %w", err,
			)
		}
	}

	err := pol.GetOrCreate(ctx, db)
	if err != nil {
		return 0, err
	}

	policyKeyCache.Store(polKey, pol.KeyID)

	return pol.KeyID, nil
}

type errorMessage struct {
	Message string `json:"message"`
}

// writeErrMsgJSON wraps the given message in JSON like `{"message": <>}` and
// writes the response, setting the header to the given code. Since this message
// will be read by the user, take care not to leak any sensitive details that
// might be in the error message.
func writeErrMsgJSON(w http.ResponseWriter, message string, code int) {
	msg := errorMessage{Message: message}

	resp, err := json.Marshal(msg)
	if err != nil {
		log.Error(err, "error marshaling error message", "message", message)
	}

	w.WriteHeader(code)

	if _, err := w.Write(resp); err != nil {
		log.Error(err, "error writing error message")
	}
}