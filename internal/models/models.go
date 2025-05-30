package models

import (
    "time"
)

type CallState string

const (
    CallStateActive     CallState = "ACTIVE"
    CallStateForwarded  CallState = "FORWARDED_TO_S3"
    CallStateReturned   CallState = "RETURNED_FROM_S3"
    CallStateCompleted  CallState = "COMPLETED_AT_S4"
    CallStateFailed     CallState = "FAILED"
)

type CallRecord struct {
    ID            int64
    CallID        string
    OriginalANI   string
    OriginalDNIS  string
    AssignedDID   string
    Status        CallState
    StartTime     time.Time
    EndTime       *time.Time
    Duration      int
    RecordingPath string
}

type CallResponse struct {
    Status      string `json:"status"`
    DIDAssigned string `json:"did_assigned"`
    NextHop     string `json:"next_hop"`
    ANIToSend   string `json:"ani_to_send"`
    DNISToSend  string `json:"dnis_to_send"`
}

type DID struct {
    ID          int
    DID         string
    InUse       bool
    Destination string
    Country     string
    UpdatedAt   time.Time
}
