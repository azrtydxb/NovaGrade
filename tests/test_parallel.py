import time

from examgrader.parallel import map_ordered


def test_map_ordered_serial():
    assert map_ordered(lambda x: x * 2, [1, 2, 3], max_workers=1) == [2, 4, 6]


def test_map_ordered_empty():
    assert map_ordered(lambda x: x, [], max_workers=8) == []


def test_map_ordered_preserves_order_under_concurrency():
    # Earlier items sleep longer, so completion order is reversed; the result
    # must still come back in input order.
    def slow(x):
        time.sleep(0.05 * (5 - x))
        return x * 10

    assert map_ordered(slow, [1, 2, 3, 4], max_workers=4) == [10, 20, 30, 40]


def test_map_ordered_actually_concurrent():
    # 4 tasks of 0.1s each should finish well under the 0.4s serial time.
    def slow(_):
        time.sleep(0.1)
        return 1

    start = time.perf_counter()
    map_ordered(slow, [0, 1, 2, 3], max_workers=4)
    elapsed = time.perf_counter() - start
    assert elapsed < 0.3  # concurrent, not ~0.4s serial
