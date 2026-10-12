"""12. NER false positives on generic words ("Client", "Contact", "Billing") are not pseudonymized;
real names (incl. a lone surname) still are.  Needs the real Presidio/spaCy stack and
PALENA_PSEUDONYMIZER_COMMON_WORDS_FILE (scripts/_env.sh)."""
import pytest

from helpers import ANTHROPIC, new_sid, upstream, dumps

GENERIC = ["Client", "Contact", "Billing", "Project", "Team", "Codebase"]
NAMES = ["Николай Петров", "Коля", "Святослав Иванов", "Sviatoslav Ivanov", "Ярослав Кузнецов",
         "Margaret Holloway", "Nikolai Petrov", "Petrov", "Holloway"]


def last_upstream_text():
    return dumps(upstream("/v1/messages")[-1]["body"])


@pytest.mark.parametrize("word", GENERIC)
def test_generic_word_not_masked(word):
    sid = new_sid("cw")
    text = f"{word}: please open the {word} notes and tell me who handles the {word} side."
    assert ANTHROPIC.send(sid, ANTHROPIC.body([("user", text)])).status == 200
    assert word in last_upstream_text()
    assert f"<PERSON_" not in last_upstream_text() and "<ORGANIZATION_" not in last_upstream_text()


@pytest.mark.parametrize("name", NAMES)
def test_real_name_still_masked(name):
    sid = new_sid("cw")
    text = f"Maintainer of the repository: {name}. Ask {name} about billing."
    assert ANTHROPIC.send(sid, ANTHROPIC.body([("user", text)])).status == 200
    assert name not in last_upstream_text(), name
