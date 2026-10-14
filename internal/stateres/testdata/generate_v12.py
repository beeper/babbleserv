"""Regenerate v12 cases with Synapse 1.161.0 (run in its Python environment).

Converts the existing small fork corpus to v12: new event IDs, implicit create,
creator-free power levels, and exact auth difference plus conflicted subgraph
from independent graph walks. No server or database is required. Events are
written as rows in synapse_forks.json's format, under a fork-level room ID.
"""
import asyncio
import copy
import json
from pathlib import Path

import synapse.server  # noqa: F401 (initialize imports)
from synapse.api.room_versions import RoomVersions
from synapse.events import make_event_from_dict
from synapse.state import v2
from synapse.storage.databases.main.event_federation import StateDifference


class Clock:
    async def sleep(self, *args):
        pass


class Store:
    def __init__(self, events):
        self.events = events

    async def get_events(self, ids, allow_rejected=False):
        return {i: self.events[i] for i in ids if i in self.events and
                (allow_rejected or self.events[i].rejected_reason is None)}

    def chain(self, roots):
        seen, stack = set(), list(roots)
        while stack:
            at = stack.pop()
            if at in seen:
                continue
            seen.add(at)
            stack.extend(self.events[at].auth_event_ids())
        return seen

    async def get_auth_chain_difference(self, room_id, states, conflicts, additional):
        chains = [self.chain(s) for s in states]
        difference = set.union(*chains) - set.intersection(*chains)
        backward = self.chain(conflicts)
        subgraph = {at for at in backward if self.chain([at]).intersection(conflicts)}
        return StateDifference(auth_difference=difference, conflicted_subgraph=subgraph)


root = Path(__file__).parent
out = []
for case in json.loads((root / 'synapse_forks.json').read_text())['forks']:
    events, mapped, room_id = {}, {}, None
    rows = []
    for original in case['events']:
        alias, typ, state_key, sender, content, auth, ts, *rejected = copy.deepcopy(original)
        if typ == 'm.room.create':
            content['room_version'] = '12'
        if typ == 'm.room.power_levels':
            content.get('users', {}).pop('@a:x', None)
        data = dict(type=typ, state_key=state_key, sender=sender, content=content,
                    auth_events=[mapped[a] for a in auth if a != '$create'],
                    prev_events=[mapped['$create']] if alias == '$alice' else [],
                    origin_server_ts=ts, depth=len(events), hashes={'sha256': alias})
        if room_id is not None:
            data['room_id'] = room_id
        ev = make_event_from_dict(data, RoomVersions.V12,
                                  rejected_reason='rejected' if rejected and rejected[0] else None)
        if room_id is None:
            room_id = ev.room_id
        mapped[alias] = ev.event_id
        events[ev.event_id] = ev
        rows.append([ev.event_id, typ, state_key, sender, content, data['auth_events'], ts]
                    + ([True] if rejected and rejected[0] else []))
    states = [{(events[mapped[a]].type, events[mapped[a]].state_key): mapped[a] for a in s}
              for s in case['states']]
    resolved = asyncio.run(v2.resolve_events_with_store(
        Clock(), room_id, RoomVersions.V12, states, None, Store(events)))
    out.append(dict(name=case['name'], room_id=room_id, events=rows,
                    states=[sorted(s.values()) for s in states], resolved=sorted(resolved.values())))
(root / 'synapse_v12.json').write_text(
    '{"forks":[\n' + ',\n'.join(json.dumps(fork, separators=(',', ':')) for fork in out) + '\n]}\n')
