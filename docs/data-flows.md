# Babbleserv Data Flows

Some high level flow charts describing how routes, databases and workers interact.

## Events (rooms)

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
                                                │    EventsIterator      │                  │           
                                                │                        │        ┌─────────┴──────────┐
                                                └──────────┬─────────────┘        │                    │
                                                           │                      │   ClientRoutes     │
                                                ┌──────────▼─────────────┐        │                    │
                                                │                        │        └────────────────────┘
Federation outgoing events ◄────────────────────┤   FederationSender     │                              
                                                │                        │                              
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
                                    │  DeviceChangeIterator  │ │ EventsIterator │ │   ClientRoutes     │
                                    │                        │ │                │ │                    │
Federation outgoing                 └─────────────┬──────────┘ └┬───────────────┘ └────────────────────┘
    - m.device_list_update                        │             │                           ▲           
    - m.signing_key_update                        │  to-device  │                           │           
      ▲                                           │             │                           │           
      │                                           │             │                           │           
      │            ┌───────────────────────┐    ┌─▼─────────────▼──────────┐                │           
      │            │                       │    │                          │  sync device_lists         
      └────────────┼  FederationSender     │◄───┤   TransientDatabase      ├────────────────┘           
                   │                       │    │                          │                            
                   └───────────────────────┘    └──────────────────────────┘                            
```
