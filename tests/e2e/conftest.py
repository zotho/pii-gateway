import pytest
import httpx

from helpers import LITELLM, MOCK, mock_reset, new_sid, stack_up, wait_ready


def pytest_configure(config):
    config.addinivalue_line("markers", "slow: restarts services (palena/redis); deselect with -m 'not slow'")


@pytest.fixture(scope="session", autouse=True)
def _stack():
    try:
        httpx.get(f"{MOCK}/_health", timeout=3).raise_for_status()
        wait_ready(30)
    except Exception:
        stack_up()
    yield
    # leave the stack up whatever the tests did
    try:
        wait_ready(5)
    except Exception:
        stack_up()


@pytest.fixture(autouse=True)
def _reset_mock():
    mock_reset()
    yield


@pytest.fixture
def sid(request):
    return new_sid(request.node.name[:20].replace("[", "").replace("]", "").replace("-", "")[:12])
