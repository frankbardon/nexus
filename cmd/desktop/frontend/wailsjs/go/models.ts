export namespace desktop {
	
	export class AgentInfo {
	    id: string;
	    name: string;
	    description: string;
	    icon: string;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new AgentInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.icon = source["icon"];
	        this.status = source["status"];
	    }
	}
	export class FieldValidation {
	    regex?: string;
	    min?: number;
	    max?: number;
	    message?: string;
	
	    static createFrom(source: any = {}) {
	        return new FieldValidation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.regex = source["regex"];
	        this.min = source["min"];
	        this.max = source["max"];
	        this.message = source["message"];
	    }
	}
	export class FileInfo {
	    name: string;
	    path: string;
	    size: number;
	    // Go type: time
	    modified: any;
	    is_dir: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FileInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.size = source["size"];
	        this.modified = this.convertValues(source["modified"], null);
	        this.is_dir = source["is_dir"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectOption {
	    value: string;
	    display: string;
	
	    static createFrom(source: any = {}) {
	        return new SelectOption(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.value = source["value"];
	        this.display = source["display"];
	    }
	}
	export class RewindResultInfo {
	    archive_name: string;
	    truncated_seq: number;
	    events_kept: number;
	    events_archived: number;

	    static createFrom(source: any = {}) {
	        return new RewindResultInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.archive_name = source["archive_name"];
	        this.truncated_seq = source["truncated_seq"];
	        this.events_kept = source["events_kept"];
	        this.events_archived = source["events_archived"];
	    }
	}
	export class TimelineEvent {
	    seq: number;
	    ts: string;
	    type: string;
	    source?: string;
	    event_id?: string;
	    parent_seq?: number;
	    side_effect?: boolean;
	    vetoed?: boolean;

	    static createFrom(source: any = {}) {
	        return new TimelineEvent(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.ts = source["ts"];
	        this.type = source["type"];
	        this.source = source["source"];
	        this.event_id = source["event_id"];
	        this.parent_seq = source["parent_seq"];
	        this.side_effect = source["side_effect"];
	        this.vetoed = source["vetoed"];
	    }
	}
	export class TimelineEventDetail extends TimelineEvent {
	    payload?: any;

	    static createFrom(source: any = {}) {
	        return new TimelineEventDetail(source);
	    }

	    constructor(source: any = {}) {
	        super(source);
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.payload = source["payload"];
	    }
	}
	export class SessionMeta {
	    id: string;
	    agent_id: string;
	    title: string;
	    status: string;
	    // Go type: time
	    created_at: any;
	    // Go type: time
	    updated_at: any;
	    preview?: any;
	
	    static createFrom(source: any = {}) {
	        return new SessionMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.agent_id = source["agent_id"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.created_at = this.convertValues(source["created_at"], null);
	        this.updated_at = this.convertValues(source["updated_at"], null);
	        this.preview = source["preview"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsFieldInfo {
	    key: string;
	    display: string;
	    description?: string;
	    type: string;
	    secret?: boolean;
	    required?: boolean;
	    default?: any;
	    validation?: FieldValidation;
	    options?: SelectOption[];
	
	    static createFrom(source: any = {}) {
	        return new SettingsFieldInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.display = source["display"];
	        this.description = source["description"];
	        this.type = source["type"];
	        this.secret = source["secret"];
	        this.required = source["required"];
	        this.default = source["default"];
	        this.validation = this.convertValues(source["validation"], FieldValidation);
	        this.options = this.convertValues(source["options"], SelectOption);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsSchema {
	    shell: SettingsFieldInfo[];
	    agents: Record<string, Array<SettingsFieldInfo>>;
	
	    static createFrom(source: any = {}) {
	        return new SettingsSchema(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.shell = this.convertValues(source["shell"], SettingsFieldInfo);
	        this.agents = this.convertValues(source["agents"], Array<SettingsFieldInfo>, true);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

