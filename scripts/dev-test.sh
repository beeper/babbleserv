#!/usr/bin/env bash

set -euo pipefail

echo "Create room..."
resp=$(
    curl 'https://localhost:5000/_matrix/client/v3/createRoom' \
        -sfk \
        -X POST \
        -d '{"preset":"private_chat","name":"my room name","topic":"YEAH","invite":["@otheruser:localhost"]}' \
        -H 'Content-Type: application/json' \
        -H 'Authorization: Bearer testuser' \
)
roomid=$(echo "$resp" | jq -r .room_id)
echo "room_id=$roomid"

echo
echo "Invite othother user..."
curl "https://localhost:5000/_matrix/client/v3/rooms/$roomid/state/m.room.member/@otherotheruser:babbleserv-dev.fizzadar.com" \
    -sfk \
    -X PUT \
    -d '{"membership":"invite"}' \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer testuser'

echo
echo "Invite with invalid user..."
curl "https://localhost:5000/_matrix/client/v3/rooms/$roomid/state/m.room.member/@otherotheruser:babbleserv-dev.fizzadar.com" \
    -sfk \
    -X PUT \
    -d '{"membership":"invite"}' \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer notauser' && exit 1 || echo "failed as expected"

echo
echo "Join as otherother user..."
curl "https://localhost:5000/_matrix/client/v3/rooms/$roomid/state/m.room.member/@otherotheruser:babbleserv-dev.fizzadar.com" \
    -sfk \
    -X PUT \
    -d '{"membership":"join"}' \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer otherotheruser'

echo
echo "Send message as otherother user..."
resp=$(
    curl "https://localhost:5000/_matrix/client/v3/rooms/$roomid/send/m.room.message/blahblah" \
        -sfk \
        -X PUT \
        -d '{"body":"AIMTHEKING"}' \
        -H 'Content-Type: application/json' \
        -H 'Authorization: Bearer otherotheruser'
)
eventid=$(echo "$resp" | jq -r .event_id)
echo "event_id=$eventid"

echo
echo "Send receipt as otherother user..."
curl "https://localhost:5000/_matrix/client/v3/rooms/$roomid/receipt/m.read/$eventid" \
    -sfk \
    -X POST \
    -d '{"body":"AIMTHEKING"}' \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer otherotheruser'
