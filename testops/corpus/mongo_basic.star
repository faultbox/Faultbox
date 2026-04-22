# testops/corpus/mongo_basic.star — MongoDB mock in isolation.
#
# Exercises mongo.server: driver handshake (hello / buildInfo) plus a
# find on a seeded collection. Catches drift in the OP_MSG responder
# and the mongo mock wrapper independently of mock_demo.
#
# Run directly:  faultbox test testops/corpus/mongo_basic.star

load("@faultbox/mocks/mongodb.star", "mongo")

users_db = mongo.server(
    name      = "users",
    interface = interface("main", "mongodb", 27028),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob",   "role": "user"},
        ],
        "audit": [
            {"_id": "a1", "event": "login", "user": "alice"},
        ],
    },
)

def test_find_returns_seeded_users():
    resp = users_db.main.find(collection = "users", filter = {})
    assert_eq(resp.status, 0)

def test_find_on_second_collection():
    resp = users_db.main.find(collection = "audit", filter = {})
    assert_eq(resp.status, 0)
