from concurrent.futures import ThreadPoolExecutor


def map_ordered(fn, items, max_workers):
    """Apply `fn` to each item, returning results in input order.

    Runs concurrently in a thread pool when `max_workers > 1` and there is more
    than one item; otherwise runs serially. `fn` must handle its own exceptions —
    a raising `fn` propagates out of this call.
    """
    items = list(items)
    if max_workers <= 1 or len(items) <= 1:
        return [fn(x) for x in items]
    with ThreadPoolExecutor(max_workers=max_workers) as executor:
        return list(executor.map(fn, items))
