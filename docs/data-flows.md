# Babbleserv Data Flows

Some high level flow charts describing how routes, databases and workers interact.

## Room Events

```
                     ┌───────────────────┐     ┌──────────────────────────┐                             
                     │                   │     │                          │                             
      ┌─────────────►│ FederationRoutes  ├────►│  RoomsDatabase           │◄────────────────┐           
      │              │                   │     │   - SendLocalEvents      │                 │           
      │              └───────────────────┘     │   - SendFederatedEvents  │             send events        
      │                                        │                          │                 │           
      │                                        └───────────┬──────────────┘                 │           
      │                                                    │                                │           
      │                                         ┌──────────▼─────────────┐                  │           
Federation Transaction PDUs                     │                        │                  │           
                                                │     EventsIterator     │                  │           
                                                │                        │        ┌─────────┴──────────┐
                                                └──────────┬─────────────┘        │                    │
                                                           │                      │    ClientRoutes    │
                                                ┌──────────▼─────────────┐        │                    │
                                                │                        │        └────────────────────┘
Federation outgoing events ◄────────────────────┤    FederationSender    │                              
                                                │      (per server)      │                              
                                                └────────────────────────┘                              
```

## Key & device management (user xs keys, device list updates)

```
                                                         ┌────────────────┐                             
                                                         │                │                             
                              ┌─────────────────────────►│ RoomsDatabase  │◄───────────────────────┐    
                              │                          │                │                        │    
                              │                          └───────────┬────┘                        │    
                              │                                      │                 user send events 
                     ┌────────┼──────────┐  ┌───────────────────┐    │                             │    
                     │                   │  │                   │    │                             │    
      ┌─────────────►│ FederationRoutes  ├─►│ AccountsDatabase  │◄───┼───────────────┐             │    
      │              │                   │  │                   │    │               │             │    
      │              └───────────────────┘  └──────┬────────────┘    │           user upload keys  │    
      │                                            │           member events         │             │    
      │                                            │                 │               │             │    
      │                                            │device change    │               │             │    
      │                                            │                 │               │             │    
Federation Transaction EDUs                        │                 │               │             │    
                                                   │                 │               │             │    
                                    ┌──────────────▼─────────┐ ┌─────▼──────────┐ ┌──┴─────────────┴───┐
                                    │                        │ │                │ │                    │
                                    │  DeviceChangeIterator  │ │ EventsIterator │ │    ClientRoutes    │
                                    │                        │ │                │ │                    │
Federation outgoing                 └─────────────┬──────────┘ └┬───────────────┘ └────────────────────┘
    - m.device_list_update                        │             │                           ▲           
    - m.signing_key_update                        │  to-device  │                           │           
      ▲                                           │             │                           │           
      │                                           │             │                           │           
      │            ┌───────────────────────┐    ┌─▼─────────────▼──────────┐                │           
      │            │                       │    │                          │  sync device_lists         
      └────────────┼    FederationSender   │◄───┤    TransientDatabase     ├────────────────┘           
                   │      (per server)     │    │                          │                            
                   └───────────────────────┘    └──────────────────────────┘                            
```

## Presence

```
                                      ┌──────────────────────────┐                                       
                                      │                          │                                       
                                      │ PresenceTimeoutIterator  │                                       
                                      │                          │                                       
                                      └─────┬─────────────▲──────┘                                       
┌────────────────────┐                      │             │                                              
│                    │              presence changes   timeout checks                                    
│ FederationRoutes   │ m.presence EDUs      │             │                                              
│                    ┼─────────────┐        │             │                       
└────────────────────┘             │    ┌───▼─────────────┼───┐     incoming reqs      ┌────────────────┐
                                   └────►                     ◄────────────────────────|                │
                                        │  TransientDatabase  │                        │ ClientRoutes   │
┌────────────────────┐             ┌────┼                     |────────────────────────►                │
│                    │             │    └───┬─────────────▲───┘  sync presence updates └────────────────┘
│ FederationSender   ◄─────────────┘   presence changes   │       from to-device evs                  
│                    │ m.presence EDUs      │             │                                              
└────────────────────┘ from to-device evs   │             │                                              
                                            │     to-device events                                       
                                      ┌─────▼─────────────┼──────┐                                       
                                      │                          │                                       
                                      │  PresenceChangeIterator  │                                       
                                      │                          │                                       
                                      └──────────────────────────┘                                       
```
