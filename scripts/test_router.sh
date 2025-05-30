#!/bin/bash

echo "Testing S2 Router API..."

# Test health
echo "1. Testing health endpoint:"
curl -s http://localhost:8001/api/health | jq .

# Test stats
echo -e "\n2. Testing stats endpoint:"
curl -s http://localhost:8001/api/stats | jq .

# Test incoming call
echo -e "\n3. Testing incoming call:"
curl -s "http://localhost:8001/api/processIncoming?callid=test123&ani=1234567890&dnis=0987654321" | jq .

# Test return call (this will fail without a valid DID)
echo -e "\n4. Testing return call:"
curl -s "http://localhost:8001/api/processReturn?ani2=0987654321&did=12125551001" | jq .
