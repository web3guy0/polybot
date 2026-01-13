#!/bin/bash
# Fetch complete whale trade history with pagination

WHALE_ADDR="$1"
OUTPUT_FILE="$2"
LIMIT=500
OFFSET=0
TOTAL=0

echo "Fetching trades for $WHALE_ADDR..."
echo "[" > "$OUTPUT_FILE"
FIRST=true

while true; do
  BATCH=$(curl -s "https://data-api.polymarket.com/activity?user=${WHALE_ADDR}&limit=${LIMIT}&offset=${OFFSET}")
  COUNT=$(echo "$BATCH" | jq 'length')
  
  if [ "$COUNT" -eq "0" ] || [ "$COUNT" == "null" ] || [ -z "$COUNT" ]; then
    break
  fi
  
  echo "  Offset $OFFSET: $COUNT records"
  
  # Append each record
  if [ "$FIRST" = true ]; then
    FIRST=false
  else
    echo "," >> "$OUTPUT_FILE"
  fi
  
  echo "$BATCH" | jq -c '.[]' | paste -sd',' >> "$OUTPUT_FILE"
  
  TOTAL=$((TOTAL + COUNT))
  OFFSET=$((OFFSET + LIMIT))
  
  # Stop if we got less than limit (end of data)
  if [ "$COUNT" -lt "$LIMIT" ]; then
    break
  fi
  
  sleep 0.2  # Rate limiting
done

echo "]" >> "$OUTPUT_FILE"
echo "Done! Total records: $TOTAL"
