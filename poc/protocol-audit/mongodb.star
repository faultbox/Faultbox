# MongoDB: every step result asserted, against a server that requires auth.
#
# The official image only enables authentication when both
# MONGO_INITDB_ROOT_USERNAME and MONGO_INITDB_ROOT_PASSWORD are set — so this
# topology is the one that exercises the credential path added in v0.16.1.
# Without it every command comes back "command requires authentication", which
# lands in the step result rather than as a connection error.

db = service("mongo",
    interface("main", "mongodb", 27017),
    image = "mongo:7",
    env = {
        "MONGO_INITDB_ROOT_USERNAME": "root",
        "MONGO_INITDB_ROOT_PASSWORD": "faultbox",
    },
    healthcheck = ready(timeout = "90s"),
)


def test_insert_and_find():
    r = db.main.insert(collection = "t", document = {"id": 1, "payload": "row-1"}, database = "app")
    assert_true(r.ok, "insert failed: %s" % r.error)

    r = db.main.find(collection = "t", filter = {"id": 1}, database = "app")
    assert_true(r.ok, "find failed: %s" % r.error)
    assert_true(len(r.data) == 1, "expected one document, got %s" % r.data)
    print("find returned:", r.data)

    # The value must survive the round trip AS AN INTEGER.
    #
    # The runtime used to flatten every non-string dict value to "" on the way
    # to BSON, so this document stored {"id": ""}. It passed anyway: the filter
    # above was mangled identically, matched the mangled document, and the
    # find "succeeded". Asserting on the value is what catches it.
    got = r.data[0].get("id", None)
    assert_true(got == 1,
        "id round-tripped as %r (%s), not the integer 1 — dict values are being mangled" %
        (got, type(got)))


def test_typed_values_survive():
    """Every scalar type a document can hold must come back unchanged."""
    doc = {"n": 42, "ratio": 0.5, "flag": True, "name": "alice"}
    r = db.main.insert(collection = "typed", document = doc, database = "app")
    assert_true(r.ok, "insert failed: %s" % r.error)

    r = db.main.find(collection = "typed", filter = {"name": "alice"}, database = "app")
    assert_true(r.ok, "find failed: %s" % r.error)
    assert_true(len(r.data) == 1, "expected one document, got %s" % r.data)

    d = r.data[0]
    assert_true(d.get("n", None) == 42, "int became %r" % d.get("n", None))
    assert_true(d.get("ratio", None) == 0.5, "float became %r" % d.get("ratio", None))
    assert_true(d.get("flag", None) == True, "bool became %r" % d.get("flag", None))
    assert_true(d.get("name", None) == "alice", "string became %r" % d.get("name", None))


def test_insert_many():
    """insert_many needs the runtime to hand the plugin a real list.

    Its []any type assertion could never succeed while list kwargs reached
    plugins as Starlark source text, so this path had never run.
    """
    r = db.main.insert_many(collection = "bulk",
        documents = [{"i": 1}, {"i": 2}, {"i": 3}], database = "app")
    assert_true(r.ok, "insert_many failed: %s" % r.error)

    r = db.main.count(collection = "bulk", database = "app")
    assert_true(r.ok, "count failed: %s" % r.error)
    print("bulk count:", r.data)


def test_count():
    r = db.main.insert(collection = "c", document = {"k": "v"}, database = "app")
    assert_true(r.ok, "insert failed: %s" % r.error)

    r = db.main.count(collection = "c", database = "app")
    assert_true(r.ok, "count failed: %s" % r.error)
    print("count:", r.data)


def test_command():
    r = db.main.command(cmd = {"ping": 1}, database = "admin")
    assert_true(r.ok, "ping command failed: %s" % r.error)
