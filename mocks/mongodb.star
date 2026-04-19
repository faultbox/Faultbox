# @faultbox/mocks/mongodb.star
#
# Starlark wrapper around mock_service() for MongoDB mocks. Stands up an
# in-process MongoDB OP_MSG responder that handles the handshake (hello /
# isMaster / buildInfo) plus find / insert / update / delete so real mongo
# drivers complete their setup and read seeded documents.
#
# Usage:
#
#     load("@faultbox/mocks/mongodb.star", "mongo")
#
#     users_db = mongo.server(
#         name      = "users-stub",
#         interface = interface("main", "mongodb", 27017),
#         collections = {
#             "users": [
#                 {"_id": "1", "name": "alice", "role": "admin"},
#                 {"_id": "2", "name": "bob", "role": "user"},
#             ],
#         },
#     )
#
# Scope (v0.8):
#
# - Handshake works — real driver connects.
# - find / findOne return seeded documents.
# - insert / update / delete acknowledge with ok:1 but do not mutate state.
#   Designed for read-through tests; for round-trip CRUD use the real server.
# - Unknown commands return ok:1 (lenient) so unusual driver chatter does
#   not fail the test.

def _server(name, interface, collections = {}, depends_on = []):
    return mock_service(
        name,
        interface,
        config = {"collections": collections},
        depends_on = depends_on,
    )

mongo = struct(
    server = _server,
)
