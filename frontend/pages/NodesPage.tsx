import { Loading } from 'f61ui/component/loading';
import { shouldAlwaysSucceed } from 'f61ui/utils';
import { getNodes } from 'generated/stoserver/stoservertypes_endpoints';
import { Node } from 'generated/stoserver/stoservertypes_types';
import { SettingsLayout } from 'layout/settingslayout';
import * as React from 'react';

interface NodesPageState {
	nodes?: Node[];
}

export default class NodesPage extends React.Component<{}, NodesPageState> {
	state: NodesPageState = {};

	componentDidMount() {
		shouldAlwaysSucceed(this.fetchData());
	}

	componentWillReceiveProps() {
		shouldAlwaysSucceed(this.fetchData());
	}

	render() {
		return (
			<SettingsLayout title="Nodes" breadcrumbs={[]}>
				{this.renderData()}
			</SettingsLayout>
		);
	}

	private renderData() {
		const nodes = this.state.nodes;

		if (!nodes) {
			return <Loading />;
		}

		const toRow = (obj: Node) => (
			<tr key={obj.Id}>
				<td>{obj.Id}</td>
				<td>{obj.Addr}</td>
				<td>{obj.Name}</td>
			</tr>
		);

		return (
			<table className="table table-striped table-hover">
				<thead>
					<tr>
						<th>Id</th>
						<th>Addr</th>
						<th>Name</th>
					</tr>
				</thead>
				<tbody>{nodes.map(toRow)}</tbody>
			</table>
		);
	}

	private async fetchData() {
		const nodes = await getNodes();

		this.setState({ nodes });
	}
}
