package router

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "log"
    "math/rand"
    "sync"
    "time"
    
    _ "github.com/go-sql-driver/mysql"
    "github.com/asterisk-call-routing-v2/internal/models"
)

type Router struct {
    db              *sql.DB
    mu              sync.RWMutex
    activeCallsMap  map[string]*models.CallRecord  // CallID -> CallRecord
    didToCallMap    map[string]string              // DID -> CallID
    recordingPath   string
}

func NewRouter(dsn string) (*Router, error) {
    db, err := sql.Open("mysql", dsn)
    if err != nil {
        return nil, err
    }
    
    if err := db.Ping(); err != nil {
        return nil, err
    }
    
    // Set connection pool settings
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(5 * time.Minute)
    
    // Create tables if not exist
    if err := createTables(db); err != nil {
        return nil, err
    }
    
    r := &Router{
        db:             db,
        activeCallsMap: make(map[string]*models.CallRecord),
        didToCallMap:   make(map[string]string),
        recordingPath:  "/var/spool/asterisk/recordings",
    }
    
    // Restore active calls from database
    if err := r.restoreActiveCalls(); err != nil {
        log.Printf("[ROUTER] Warning: Failed to restore active calls: %v", err)
    }
    
    // Start cleanup goroutine
    go r.cleanupRoutine()
    
    return r, nil
}

func createTables(db *sql.DB) error {
    queries := []string{
        `CREATE TABLE IF NOT EXISTS call_records (
            id BIGINT AUTO_INCREMENT PRIMARY KEY,
            call_id VARCHAR(100) UNIQUE NOT NULL,
            original_ani VARCHAR(50),
            original_dnis VARCHAR(50),
            assigned_did VARCHAR(50),
            status VARCHAR(50),
            start_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            end_time TIMESTAMP NULL,
            duration INT DEFAULT 0,
            recording_path VARCHAR(255),
            INDEX idx_call_id (call_id),
            INDEX idx_did (assigned_did),
            INDEX idx_status (status),
            INDEX idx_start_time (start_time)
        )`,
        `CREATE TABLE IF NOT EXISTS dids (
            id INT AUTO_INCREMENT PRIMARY KEY,
            did VARCHAR(50) UNIQUE NOT NULL,
            in_use BOOLEAN DEFAULT FALSE,
            destination VARCHAR(50),
            country VARCHAR(50),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
            INDEX idx_in_use (in_use)
        )`,
    }
    
    for _, query := range queries {
        if _, err := db.Exec(query); err != nil {
            return err
        }
    }
    
    return nil
}

// ProcessIncomingCall handles initial calls from S1 (Step 1 -> Step 2)
func (r *Router) ProcessIncomingCall(callID, ani, dnis string) (*models.CallResponse, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    log.Printf("[ROUTER] === STEP 1->2: Processing incoming call ===")
    log.Printf("[ROUTER] CallID: %s, ANI-1: %s, DNIS-1: %s", callID, ani, dnis)
    
    // Get available DID
    did, err := r.getAvailableDID()
    if err != nil {
        log.Printf("[ROUTER] Failed to get available DID: %v", err)
        return nil, err
    }
    
    // Mark DID as in use
    if err := r.markDIDInUse(did, dnis); err != nil {
        log.Printf("[ROUTER] Failed to mark DID in use: %v", err)
        return nil, err
    }
    
    // Create call record
    record := &models.CallRecord{
        CallID:       callID,
        OriginalANI:  ani,
        OriginalDNIS: dnis,
        AssignedDID:  did,
        Status:       models.CallStateActive,
        StartTime:    time.Now(),
        RecordingPath: fmt.Sprintf("%s/%s.wav", r.recordingPath, callID),
    }
    
    // Store in memory
    r.activeCallsMap[callID] = record
    r.didToCallMap[did] = callID
    
    // Store in database
    if err := r.storeCallRecord(record); err != nil {
        log.Printf("[ROUTER] Failed to store call record: %v", err)
    }
    
    // According to workflow: ANI-2 = DNIS-1, DID is the new destination
    response := &models.CallResponse{
        Status:      "success",
        DIDAssigned: did,
        NextHop:     "trunk-s3",
        ANIToSend:   dnis,      // DNIS-1 becomes ANI-2
        DNISToSend:  did,       // DID becomes destination
    }
    
    log.Printf("[ROUTER] === TRANSFORMATION: ANI-1=%s, DNIS-1=%s -> ANI-2=%s, DID=%s ===", 
        ani, dnis, response.ANIToSend, response.DNISToSend)
    
    // Update status
    r.updateCallStatus(callID, models.CallStateForwarded)
    
    return response, nil
}

// ProcessReturnCall handles calls returning from S3 (Step 3 -> Step 4)
func (r *Router) ProcessReturnCall(ani2, did string) (*models.CallResponse, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    log.Printf("[ROUTER] === STEP 3->4: Processing return call ===")
    log.Printf("[ROUTER] ANI-2: %s, DID: %s", ani2, did)
    
    // Clean DID string (remove any newlines or spaces)
    did = cleanString(did)
    ani2 = cleanString(ani2)
    
    // Find call by DID
    callID, exists := r.didToCallMap[did]
    if !exists {
        log.Printf("[ROUTER] DID %s not found in memory, checking database", did)
        // Try to find in database
        record, err := r.getCallRecordByDID(did)
        if err != nil {
            log.Printf("[ROUTER] No record found for DID %s: %v", did, err)
            return nil, fmt.Errorf("no active call for DID %s", did)
        }
        callID = record.CallID
        // Restore to memory
        r.activeCallsMap[callID] = record
        r.didToCallMap[did] = callID
        log.Printf("[ROUTER] Restored call %s from database", callID)
    }
    
    // Get call record
    record, exists := r.activeCallsMap[callID]
    if !exists {
        return nil, fmt.Errorf("call record not found for callID %s", callID)
    }
    
    // Verify ANI-2 matches original DNIS-1
    if ani2 != record.OriginalDNIS {
        log.Printf("[ROUTER] WARNING: ANI mismatch - expected %s, got %s", record.OriginalDNIS, ani2)
    }
    
    // Update status
    r.updateCallStatus(callID, models.CallStateReturned)
    
    // Return original ANI and DNIS for forwarding to S4
    response := &models.CallResponse{
        Status:     "success",
        NextHop:    "trunk-s4",
        ANIToSend:  record.OriginalANI,   // Restore original ANI-1
        DNISToSend: record.OriginalDNIS,  // Restore original DNIS-1
    }
    
    log.Printf("[ROUTER] === RESTORATION: ANI-2=%s, DID=%s -> ANI-1=%s, DNIS-1=%s ===", 
        ani2, did, response.ANIToSend, response.DNISToSend)
    
    return response, nil
}

// Helper methods

func (r *Router) getAvailableDID() (string, error) {
    query := `
        SELECT did FROM dids 
        WHERE in_use = 0 
        ORDER BY RAND() 
        LIMIT 1
        FOR UPDATE
    `
    
    var did string
    err := r.db.QueryRow(query).Scan(&did)
    if err != nil {
        return "", fmt.Errorf("no available DIDs: %v", err)
    }
    
    return did, nil
}

func (r *Router) markDIDInUse(did, destination string) error {
    query := `
        UPDATE dids 
        SET in_use = 1, destination = ?, updated_at = NOW()
        WHERE did = ?
    `
    
    result, err := r.db.Exec(query, destination, did)
    if err != nil {
        return err
    }
    
    rows, _ := result.RowsAffected()
    if rows == 0 {
        return fmt.Errorf("DID not found: %s", did)
    }
    
    return nil
}

func (r *Router) releaseDID(did string) error {
    query := `
        UPDATE dids 
        SET in_use = 0, destination = NULL, updated_at = NOW()
        WHERE did = ?
    `
    
    _, err := r.db.Exec(query, did)
    return err
}

func (r *Router) storeCallRecord(record *models.CallRecord) error {
    query := `
        INSERT INTO call_records 
        (call_id, original_ani, original_dnis, assigned_did, status, start_time, recording_path)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE
        status = VALUES(status),
        assigned_did = VALUES(assigned_did),
        updated_at = NOW()
    `
    
    _, err := r.db.Exec(query, 
        record.CallID, 
        record.OriginalANI, 
        record.OriginalDNIS,
        record.AssignedDID, 
        record.Status, 
        record.StartTime,
        record.RecordingPath,
    )
    
    return err
}

func (r *Router) updateCallStatus(callID string, status models.CallState) error {
    query := `
        UPDATE call_records 
        SET status = ?, 
            end_time = CASE WHEN ? IN ('COMPLETED_AT_S4', 'FAILED') THEN NOW() ELSE end_time END,
            duration = CASE WHEN ? IN ('COMPLETED_AT_S4', 'FAILED') THEN TIMESTAMPDIFF(SECOND, start_time, NOW()) ELSE duration END
        WHERE call_id = ?
    `
    
    _, err := r.db.Exec(query, status, status, status, callID)
    return err
}

func (r *Router) getCallRecordByDID(did string) (*models.CallRecord, error) {
    query := `
        SELECT call_id, original_ani, original_dnis, assigned_did, status, start_time, recording_path
        FROM call_records
        WHERE assigned_did = ? 
        AND status IN ('ACTIVE', 'FORWARDED_TO_S3', 'RETURNED_FROM_S3')
        AND start_time > DATE_SUB(NOW(), INTERVAL 5 MINUTE)
        ORDER BY start_time DESC
        LIMIT 1
    `
    
    record := &models.CallRecord{}
    err := r.db.QueryRow(query, did).Scan(
        &record.CallID,
        &record.OriginalANI,
        &record.OriginalDNIS,
        &record.AssignedDID,
        &record.Status,
        &record.StartTime,
        &record.RecordingPath,
    )
    
    if err != nil {
        return nil, err
    }
    
    return record, nil
}

func (r *Router) restoreActiveCalls() error {
    query := `
        SELECT call_id, original_ani, original_dnis, assigned_did, status, start_time, recording_path
        FROM call_records
        WHERE status IN ('ACTIVE', 'FORWARDED_TO_S3', 'RETURNED_FROM_S3')
        AND start_time > DATE_SUB(NOW(), INTERVAL 5 MINUTE)
    `
    
    rows, err := r.db.Query(query)
    if err != nil {
        return err
    }
    defer rows.Close()
    
    count := 0
    for rows.Next() {
        record := &models.CallRecord{}
        err := rows.Scan(
            &record.CallID,
            &record.OriginalANI,
            &record.OriginalDNIS,
            &record.AssignedDID,
            &record.Status,
            &record.StartTime,
            &record.RecordingPath,
        )
        
        if err != nil {
            log.Printf("[ROUTER] Error scanning record: %v", err)
            continue
        }
        
        r.activeCallsMap[record.CallID] = record
        r.didToCallMap[record.AssignedDID] = record.CallID
        count++
    }
    
    log.Printf("[ROUTER] Restored %d active calls from database", count)
    return nil
}

func (r *Router) cleanupRoutine() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    
    for range ticker.C {
        r.cleanupStaleCalls()
    }
}

func (r *Router) cleanupStaleCalls() {
    // Clean up calls older than 5 minutes
    query := `
        UPDATE call_records 
        SET status = 'FAILED', end_time = NOW()
        WHERE status IN ('ACTIVE', 'FORWARDED_TO_S3')
        AND start_time < DATE_SUB(NOW(), INTERVAL 5 MINUTE)
    `
    
    result, err := r.db.Exec(query)
    if err != nil {
        log.Printf("[ROUTER] Error cleaning up stale calls: %v", err)
        return
    }
    
    rows, _ := result.RowsAffected()
    if rows > 0 {
        log.Printf("[ROUTER] Cleaned up %d stale calls", rows)
        
        // Release DIDs
        r.db.Exec(`
            UPDATE dids d
            INNER JOIN call_records cr ON d.did = cr.assigned_did
            SET d.in_use = 0, d.destination = NULL
            WHERE cr.status = 'FAILED'
            AND cr.end_time > DATE_SUB(NOW(), INTERVAL 1 MINUTE)
        `)
    }
}

func (r *Router) GetStatistics() (map[string]interface{}, error) {
    r.mu.RLock()
    activeCalls := len(r.activeCallsMap)
    r.mu.RUnlock()
    
    stats := make(map[string]interface{})
    stats["active_calls"] = activeCalls
    
    // Get DID statistics
    var totalDIDs, usedDIDs int
    r.db.QueryRow("SELECT COUNT(*), SUM(CASE WHEN in_use = 1 THEN 1 ELSE 0 END) FROM dids").Scan(&totalDIDs, &usedDIDs)
    
    stats["total_dids"] = totalDIDs
    stats["used_dids"] = usedDIDs
    stats["available_dids"] = totalDIDs - usedDIDs
    
    // Get call statistics
    var todaysCalls, completedCalls int
    r.db.QueryRow(`
        SELECT 
            COUNT(*),
            SUM(CASE WHEN status = 'COMPLETED_AT_S4' THEN 1 ELSE 0 END)
        FROM call_records 
        WHERE DATE(start_time) = CURDATE()
    `).Scan(&todaysCalls, &completedCalls)
    
    stats["calls_today"] = todaysCalls
    stats["completed_calls"] = completedCalls
    stats["timestamp"] = time.Now().Format(time.RFC3339)
    
    // Add memory call details
    var memoryDetails []map[string]interface{}
    r.mu.RLock()
    for _, call := range r.activeCallsMap {
        memoryDetails = append(memoryDetails, map[string]interface{}{
            "call_id": call.CallID,
            "did": call.AssignedDID,
            "status": call.Status,
            "start_time": call.StartTime,
        })
    }
    r.mu.RUnlock()
    stats["memory_calls"] = memoryDetails
    
    return stats, nil
}

func (r *Router) Close() {
    if r.db != nil {
        r.db.Close()
    }
}

// Utility function to clean strings
func cleanString(s string) string {
    // Remove newlines, carriage returns, and extra spaces
    s = strings.TrimSpace(s)
    s = strings.ReplaceAll(s, "\n", "")
    s = strings.ReplaceAll(s, "\r", "")
    return s
}

// Add strings import
import (
    "database/sql"
    "encoding/json"
    "fmt"
    "log"
    "math/rand"
    "strings"
    "sync"
    "time"
    
    _ "github.com/go-sql-driver/mysql"
    "github.com/asterisk-call-routing-v2/internal/models"
)
