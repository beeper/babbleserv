# Room DAGs & Event Authorization

- everything is stored against a version
- from a db perspective everything is sorted/stored in insert order (think synapse stream ordering)
- thus, from the HS side, there is really no DAG, prev events are merely used for authentication stage 5 and, where needed, state resolution 
- current state of a room is maintained on insert at room/type/statekey
    - As described below we resolve state at insert time, so current state can always be considered resolved and valid
    - This makes local sends very very efficient
- state history is stored by room/version and also room/type/statekey/version
- this gives us
    - relatively quick “room state at version x” by iterating the entire room/version prefix up to that version
        - more state changes makes rooms slower to resolve
        - MSC state diff api would fix this
    - Very quick “state changes between x and y versions”
    - Very very quick “state of this type/key at this version”
- we use the final check there to provide extremely fast auth lookup for stage 5 event auth “state at prev events”
    - This is also where any state resolution happens - if an event specifies multiple prev events we grab the auth states/chains for each and resolve them to produce the auth state for that event
- likewise against current state for stage 6
- to do this we lookup specifically the relevant state events to auth an event, and we don’t need the whole room state ids to do that (just power levels, sender member, etc…)
